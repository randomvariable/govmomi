/*
Copyright (c) 2014-2018 VMware, Inc. All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package soap

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/vmware/govmomi/internal/version"
	"github.com/vmware/govmomi/vim25/progress"
	"github.com/vmware/govmomi/vim25/types"
	"github.com/vmware/govmomi/vim25/xml"
)

type HasFault interface {
	Fault() *Fault
}

type RoundTripper interface {
	RoundTrip(ctx context.Context, req, res HasFault) error
}

const (
	SessionCookieName = "vmware_soap_session"
)

// defaultUserAgent is the default user agent string, e.g.
// "govmomi/0.28.0 (go1.18.3;linux;amd64)"
var defaultUserAgent = fmt.Sprintf(
	"%s/%s (%s)",
	version.ClientName,
	version.ClientVersion,
	strings.Join([]string{runtime.Version(), runtime.GOOS, runtime.GOARCH}, ";"),
)

type Client struct {
	http.Client

	u *url.URL
	k bool // Named after curl's -k flag
	d *debugContainer
	t *http.Transport

	hostsMu sync.Mutex
	hosts   map[string]string

	Namespace string // Vim namespace
	Version   string // Vim version
	Types     types.Func
	UserAgent string

	cookie          string
	insecureCookies bool
}

var schemeMatch = regexp.MustCompile(`^\w+://`)

type errInvalidCACertificate struct {
	File string
}

func (e errInvalidCACertificate) Error() string {
	return fmt.Sprintf(
		"invalid certificate '%s', cannot be used as a trusted CA certificate",
		e.File,
	)
}

// ParseURL is wrapper around url.Parse, where Scheme defaults to "https" and Path defaults to "/sdk"
func ParseURL(s string) (*url.URL, error) {
	var err error
	var u *url.URL

	if s != "" {
		// Default the scheme to https
		if !schemeMatch.MatchString(s) {
			s = "https://" + s
		}

		u, err = url.Parse(s)
		if err != nil {
			return nil, err
		}

		// Default the path to /sdk
		if u.Path == "" {
			u.Path = "/sdk"
		}

		if u.User == nil {
			u.User = url.UserPassword("", "")
		}
	}

	return u, nil
}

func NewClient(u *url.URL, insecure bool) *Client {
	c := Client{
		u: u,
		k: insecure,
		d: newDebug(),

		Types: types.TypeFunc(),
	}

	// Initialize http.RoundTripper on client, so we can customize it below
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		c.t = &http.Transport{
			Proxy:                 t.Proxy,
			DialContext:           t.DialContext,
			MaxIdleConns:          t.MaxIdleConns,
			IdleConnTimeout:       t.IdleConnTimeout,
			TLSHandshakeTimeout:   t.TLSHandshakeTimeout,
			ExpectContinueTimeout: t.ExpectContinueTimeout,
		}
	} else {
		c.t = new(http.Transport)
	}

	c.hosts = make(map[string]string)
	c.t.TLSClientConfig = &tls.Config{InsecureSkipVerify: c.k}

	// Always set DialTLS and DialTLSContext, even if InsecureSkipVerify=true,
	// because of how certificate verification has been delegated to the host's
	// PKI framework in Go 1.18. Please see the following links for more info:
	//
	//   * https://tip.golang.org/doc/go1.18 (search for "Certificate.Verify")
	//   * https://github.com/square/certigo/issues/264
	c.t.DialTLSContext = c.dialTLSContext

	c.Client.Transport = c.t
	c.Client.Jar, _ = cookiejar.New(nil)

	// Remove user information from a copy of the URL
	c.u = c.URL()
	c.u.User = nil

	if c.u.Scheme == "http" {
		c.insecureCookies = os.Getenv("GOVMOMI_INSECURE_COOKIES") == "true"
	}

	return &c
}

func (c *Client) DefaultTransport() *http.Transport {
	return c.t
}

// NewServiceClient creates a NewClient with the given URL.Path and namespace.
func (c *Client) NewServiceClient(path string, namespace string) *Client {
	vc := c.URL()
	u, err := url.Parse(path)
	if err != nil {
		log.Panicf("url.Parse(%q): %s", path, err)
	}
	if u.Host == "" {
		u.Scheme = vc.Scheme
		u.Host = vc.Host
	}

	client := NewClient(u, c.k)
	client.Namespace = "urn:" + namespace
	client.DefaultTransport().TLSClientConfig = c.DefaultTransport().TLSClientConfig
	if cert := c.Certificate(); cert != nil {
		client.SetCertificate(*cert)
	}

	// Copy the trusted thumbprints
	c.hostsMu.Lock()
	for k, v := range c.hosts {
		client.hosts[k] = v
	}
	c.hostsMu.Unlock()

	// Copy the cookies
	client.Client.Jar.SetCookies(u, c.Client.Jar.Cookies(u))

	// Set SOAP Header cookie
	for _, cookie := range client.Jar.Cookies(u) {
		if cookie.Name == SessionCookieName {
			client.cookie = cookie.Value
			break
		}
	}

	// Copy any query params (e.g. GOVMOMI_TUNNEL_PROXY_PORT used in testing)
	client.u.RawQuery = vc.RawQuery

	client.UserAgent = c.UserAgent

	vimTypes := c.Types
	client.Types = func(name string) (reflect.Type, bool) {
		kind, ok := vimTypes(name)
		if ok {
			return kind, ok
		}
		// vim25/xml typeToString() does not have an option to include namespace prefix.
		// Workaround this by re-trying the lookup with the namespace prefix.
		return vimTypes(namespace + ":" + name)
	}

	return client
}

// SetRootCAs defines the set of PEM-encoded file locations of root certificate
// authorities the client uses when verifying server certificates instead of the
// TLS defaults which uses the host's root CA set. Multiple PEM file locations
// can be specified using the OS-specific PathListSeparator.
//
// See: http.Client.Transport.TLSClientConfig.RootCAs and
// https://pkg.go.dev/os#PathListSeparator
func (c *Client) SetRootCAs(pemPaths string) error {
	pool := x509.NewCertPool()

	for _, name := range filepath.SplitList(pemPaths) {
		pem, err := ioutil.ReadFile(filepath.Clean(name))
		if err != nil {
			return err
		}

		if ok := pool.AppendCertsFromPEM(pem); !ok {
			return errInvalidCACertificate{
				File: name,
			}
		}
	}

	c.t.TLSClientConfig.RootCAs = pool

	return nil
}

// Add default https port if missing
func hostAddr(addr string) string {
	_, port := splitHostPort(addr)
	if port == "" {
		return addr + ":443"
	}
	return addr
}

// SetThumbprint sets the known certificate thumbprint for the given host.
// A custom DialTLS function is used to support thumbprint based verification.
// We first try tls.Dial with the default tls.Config, only falling back to thumbprint verification
// if it fails with an x509.UnknownAuthorityError or x509.HostnameError
//
// See: http.Client.Transport.DialTLS
func (c *Client) SetThumbprint(host string, thumbprint string) {
	host = hostAddr(host)

	c.hostsMu.Lock()
	if thumbprint == "" {
		delete(c.hosts, host)
	} else {
		c.hosts[host] = thumbprint
	}
	c.hostsMu.Unlock()
}

// Thumbprint returns the certificate thumbprint for the given host if known to this client.
func (c *Client) Thumbprint(host string) string {
	host = hostAddr(host)
	c.hostsMu.Lock()
	defer c.hostsMu.Unlock()
	return c.hosts[host]
}

// LoadThumbprints from file with the give name.
// If name is empty or name does not exist this function will return nil.
func (c *Client) LoadThumbprints(file string) error {
	if file == "" {
		return nil
	}

	for _, name := range filepath.SplitList(file) {
		err := c.loadThumbprints(name)
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Client) loadThumbprints(name string) error {
	f, err := os.Open(filepath.Clean(name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	scanner := bufio.NewScanner(f)

	for scanner.Scan() {
		e := strings.SplitN(scanner.Text(), " ", 2)
		if len(e) != 2 {
			continue
		}

		c.SetThumbprint(e[0], e[1])
	}

	_ = f.Close()

	return scanner.Err()
}

// WithDialer sets the DialContext function for the client's transport, to allow for proxying via another transport.
func (c *Client) WithDialer(dialer func(ctx context.Context, network, addr string) (net.Conn, error)) *Client {
	legacyDialer := func(network, addr string) (net.Conn, error) {
		return dialer(context.Background(), network, addr)
	}

	c.t.Dial = legacyDialer
	c.t.DialContext = dialer

	return c
}

// ThumbprintSHA1 returns the thumbprint of the given cert in the same format used by the SDK and Client.SetThumbprint.
//
// See: SSLVerifyFault.Thumbprint, SessionManagerGenericServiceTicket.Thumbprint, HostConnectSpec.SslThumbprint
func ThumbprintSHA1(cert *x509.Certificate) string {
	sum := sha1.Sum(cert.Raw)
	hex := make([]string, len(sum))
	for i, b := range sum {
		hex[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(hex, ":")
}

func (c *Client) dialTLSContext(
	ctx context.Context,
	network, addr string,
) (net.Conn, error) {
	// Would be nice if there was a tls.Config.Verify func,
	// see tls.clientHandshakeState.doFullHandshake

	conn, err := tls.Dial(network, addr, c.t.TLSClientConfig)

	if err == nil {
		return conn, nil
	}

	switch err.(type) {
	case x509.UnknownAuthorityError:
	case x509.HostnameError:
	default:
		// Allow a thumbprint verification attempt if the error indicates
		// the failure was due to lack of trust.
		//
		// Please note the err variable is not a special type of x509 or HTTP
		// error that can be validated by a type assertion. The err variable is
		// in fact an *errors.errorString.
		switch {
		case strings.HasSuffix(err.Error(), "certificate is not trusted"):
			// darwin and linux
		case strings.HasSuffix(err.Error(), "certificate signed by unknown authority"):
			// windows
		default:
			return nil, err
		}
	}

	thumbprint := c.Thumbprint(addr)
	if thumbprint == "" {
		return nil, err
	}

	config := &tls.Config{InsecureSkipVerify: true}
	conn, err = tls.Dial(network, addr, config)
	if err != nil {
		return nil, err
	}

	cert := conn.ConnectionState().PeerCertificates[0]
	peer := ThumbprintSHA1(cert)
	if thumbprint != peer {
		_ = conn.Close()

		return nil, fmt.Errorf("host %q thumbprint does not match %q", addr, thumbprint)
	}

	return conn, nil
}

func (c *Client) dialTLS(network, addr string) (net.Conn, error) {
	return c.dialTLSContext(context.Background(), network, addr)
}

// splitHostPort is similar to net.SplitHostPort,
// but rather than return error if there isn't a ':port',
// return an empty string for the port.
func splitHostPort(host string) (string, string) {
	ix := strings.LastIndex(host, ":")

	if ix <= strings.LastIndex(host, "]") {
		return host, ""
	}

	name := host[:ix]
	port := host[ix+1:]

	return name, port
}

const sdkTunnel = "sdkTunnel:8089"

func (c *Client) Certificate() *tls.Certificate {
	certs := c.t.TLSClientConfig.Certificates
	if len(certs) == 0 {
		return nil
	}
	return &certs[0]
}

func (c *Client) SetCertificate(cert tls.Certificate) {
	t := c.Client.Transport.(*http.Transport)

	// Extension or HoK certificate
	t.TLSClientConfig.Certificates = []tls.Certificate{cert}
}

// Tunnel returns a Client configured to proxy requests through vCenter's http port 80,
// to the SDK tunnel virtual host.  Use of the SDK tunnel is required by LoginExtensionByCertificate()
// and optional for other methods.
func (c *Client) Tunnel() *Client {
	tunnel := c.NewServiceClient(c.u.Path, c.Namespace)
	t := tunnel.Client.Transport.(*http.Transport)
	// Proxy to vCenter host on port 80
	host := tunnel.u.Hostname()
	// Should be no reason to change the default port other than testing
	key := "GOVMOMI_TUNNEL_PROXY_PORT"

	port := tunnel.URL().Query().Get(key)
	if port == "" {
		port = os.Getenv(key)
	}

	if port != "" {
		host += ":" + port
	}

	t.Proxy = http.ProxyURL(&url.URL{
		Scheme: "http",
		Host:   host,
	})

	// Rewrite url Host to use the sdk tunnel, required for a certificate request.
	tunnel.u.Host = sdkTunnel
	return tunnel
}

func (c *Client) URL() *url.URL {
	urlCopy := *c.u
	return &urlCopy
}

type marshaledClient struct {
	Cookies  []*http.Cookie
	URL      *url.URL
	Insecure bool
	Version  string
}

func (c *Client) MarshalJSON() ([]byte, error) {
	m := marshaledClient{
		Cookies:  c.Jar.Cookies(c.u),
		URL:      c.u,
		Insecure: c.k,
		Version:  c.Version,
	}

	return json.Marshal(m)
}

func (c *Client) UnmarshalJSON(b []byte) error {
	var m marshaledClient

	err := json.Unmarshal(b, &m)
	if err != nil {
		return err
	}

	*c = *NewClient(m.URL, m.Insecure)
	c.Version = m.Version
	c.Jar.SetCookies(m.URL, m.Cookies)

	return nil
}

type kindContext struct{}

func (c *Client) setInsecureCookies(res *http.Response) {
	cookies := res.Cookies()
	if len(cookies) != 0 {
		for _, cookie := range cookies {
			cookie.Secure = false
		}
		c.Jar.SetCookies(c.u, cookies)
	}
}

func (c *Client) Do(ctx context.Context, req *http.Request, f func(*http.Response) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// Create debugging context for this round trip
	d := c.d.newRoundTrip()
	if d.enabled() {
		defer d.done()
	}

	// use default
	if c.UserAgent == "" {
		c.UserAgent = defaultUserAgent
	}

	req.Header.Set(`User-Agent`, c.UserAgent)

	ext := ""
	if d.enabled() {
		ext = d.debugRequest(req)
	}

	tstart := time.Now()
	res, err := c.Client.Do(req.WithContext(ctx))
	tstop := time.Now()

	if d.enabled() {
		var name string
		if kind, ok := ctx.Value(kindContext{}).(HasFault); ok {
			name = fmt.Sprintf("%T", kind)
		} else {
			name = fmt.Sprintf("%s %s", req.Method, req.URL)
		}
		d.logf("%6dms (%s)", tstop.Sub(tstart)/time.Millisecond, name)
	}

	if err != nil {
		return err
	}

	if d.enabled() {
		d.debugResponse(res, ext)
	}

	if c.insecureCookies {
		c.setInsecureCookies(res)
	}

	defer res.Body.Close()

	return f(res)
}

// Signer can be implemented by soap.Header.Security to sign requests.
// If the soap.Header.Security field is set to an implementation of Signer via WithHeader(),
// then Client.RoundTrip will call Sign() to marshal the SOAP request.
type Signer interface {
	Sign(Envelope) ([]byte, error)
}

type headerContext struct{}

// WithHeader can be used to modify the outgoing request soap.Header fields.
func (c *Client) WithHeader(ctx context.Context, header Header) context.Context {
	return context.WithValue(ctx, headerContext{}, header)
}

type statusError struct {
	res *http.Response
}

// Temporary returns true for HTTP response codes that can be retried
// See vim25.IsTemporaryNetworkError
func (e *statusError) Temporary() bool {
	switch e.res.StatusCode {
	case http.StatusBadGateway:
		return true
	}
	return false
}

func (e *statusError) Error() string {
	return e.res.Status
}

func newStatusError(res *http.Response) error {
	return &url.Error{
		Op:  res.Request.Method,
		URL: res.Request.URL.Path,
		Err: &statusError{res},
	}
}

func (c *Client) RoundTrip(ctx context.Context, reqBody, resBody HasFault) error {
	var err error
	var b []byte

	reqEnv := Envelope{Body: reqBody}
	resEnv := Envelope{Body: resBody}

	h, ok := ctx.Value(headerContext{}).(Header)
	if !ok {
		h = Header{}
	}

	// We added support for OperationID before soap.Header was exported.
	if id, ok := ctx.Value(types.ID{}).(string); ok {
		h.ID = id
	}

	h.Cookie = c.cookie
	if h.Cookie != "" || h.ID != "" || h.Security != nil {
		reqEnv.Header = &h // XML marshal header only if a field is set
	}

	if signer, ok := h.Security.(Signer); ok {
		b, err = signer.Sign(reqEnv)
		if err != nil {
			return err
		}
	} else {
		b, err = xml.Marshal(reqEnv)
		if err != nil {
			panic(err)
		}
	}

	rawReqBody := io.MultiReader(strings.NewReader(xml.Header), bytes.NewReader(b))
	req, err := http.NewRequest("POST", c.u.String(), rawReqBody)
	if err != nil {
		panic(err)
	}

	req.Header.Set(`Content-Type`, `text/xml; charset="utf-8"`)

	action := h.Action
	if action == "" {
		action = fmt.Sprintf("%s/%s", c.Namespace, c.Version)
	}
	req.Header.Set(`SOAPAction`, action)

	return c.Do(context.WithValue(ctx, kindContext{}, resBody), req, func(res *http.Response) error {
		switch res.StatusCode {
		case http.StatusOK:
			// OK
		case http.StatusInternalServerError:
			// Error, but typically includes a body explaining the error
		default:
			return newStatusError(res)
		}

		dec := xml.NewDecoder(res.Body)
		dec.TypeFunc = c.Types
		err = dec.Decode(&resEnv)
		if err != nil {
			return err
		}

		if f := resBody.Fault(); f != nil {
			return WrapSoapFault(f)
		}

		return err
	})
}

func (c *Client) CloseIdleConnections() {
	c.t.CloseIdleConnections()
}

// ParseURL wraps url.Parse to rewrite the URL.Host field
// In the case of VM guest uploads or NFC lease URLs, a Host
// field with a value of "*" is rewritten to the Client's URL.Host.
func (c *Client) ParseURL(urlStr string) (*url.URL, error) {
	u, err := url.Parse(urlStr)
	if err != nil {
		return nil, err
	}

	host, _ := splitHostPort(u.Host)
	if host == "*" {
		// Also use Client's port, to support port forwarding
		u.Host = c.URL().Host
	}

	return u, nil
}

type Upload struct {
	Type          string
	Method        string
	ContentLength int64
	Headers       map[string]string
	Ticket        *http.Cookie
	Progress      progress.Sinker
}

var DefaultUpload = Upload{
	Type:   "application/octet-stream",
	Method: "PUT",
}

// Upload PUTs the local file to the given URL
func (c *Client) Upload(ctx context.Context, f io.Reader, u *url.URL, param *Upload) error {
	var err error

	if param.Progress != nil {
		pr := progress.NewReader(ctx, param.Progress, f, param.ContentLength)
		f = pr

		// Mark progress reader as done when returning from this function.
		defer func() {
			pr.Done(err)
		}()
	}

	req, err := http.NewRequest(param.Method, u.String(), f)
	if err != nil {
		return err
	}

	req = req.WithContext(ctx)

	req.ContentLength = param.ContentLength
	req.Header.Set("Content-Type", param.Type)

	for k, v := range param.Headers {
		req.Header.Add(k, v)
	}

	if param.Ticket != nil {
		req.AddCookie(param.Ticket)
	}

	res, err := c.Client.Do(req)
	if err != nil {
		return err
	}

	defer res.Body.Close()

	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusCreated:
	default:
		err = errors.New(res.Status)
	}

	return err
}

// UploadFile PUTs the local file to the given URL
func (c *Client) UploadFile(ctx context.Context, file string, u *url.URL, param *Upload) error {
	if param == nil {
		p := DefaultUpload // Copy since we set ContentLength
		param = &p
	}

	s, err := os.Stat(file)
	if err != nil {
		return err
	}

	f, err := os.Open(filepath.Clean(file))
	if err != nil {
		return err
	}
	defer f.Close()

	param.ContentLength = s.Size()

	return c.Upload(ctx, f, u, param)
}

type Download struct {
	Method   string
	Headers  map[string]string
	Ticket   *http.Cookie
	Progress progress.Sinker
	Writer   io.Writer
}

var DefaultDownload = Download{
	Method: "GET",
}

// DownloadRequest wraps http.Client.Do, returning the http.Response without checking its StatusCode
func (c *Client) DownloadRequest(ctx context.Context, u *url.URL, param *Download) (*http.Response, error) {
	req, err := http.NewRequest(param.Method, u.String(), nil)
	if err != nil {
		return nil, err
	}

	req = req.WithContext(ctx)

	for k, v := range param.Headers {
		req.Header.Add(k, v)
	}

	if param.Ticket != nil {
		req.AddCookie(param.Ticket)
	}

	return c.Client.Do(req)
}

// Download GETs the remote file from the given URL
func (c *Client) Download(ctx context.Context, u *url.URL, param *Download) (io.ReadCloser, int64, error) {
	res, err := c.DownloadRequest(ctx, u, param)
	if err != nil {
		return nil, 0, err
	}

	switch res.StatusCode {
	case http.StatusOK:
	default:
		err = fmt.Errorf("download(%s): %s", u, res.Status)
	}

	if err != nil {
		return nil, 0, err
	}

	r := res.Body

	return r, res.ContentLength, nil
}

func (c *Client) WriteFile(ctx context.Context, file string, src io.Reader, size int64, s progress.Sinker, w io.Writer) error {
	var err error

	r := src

	fh, err := os.Create(file)
	if err != nil {
		return err
	}

	if s != nil {
		pr := progress.NewReader(ctx, s, src, size)
		r = pr

		// Mark progress reader as done when returning from this function.
		defer func() {
			pr.Done(err)
		}()
	}

	if w == nil {
		w = fh
	} else {
		w = io.MultiWriter(w, fh)
	}

	_, err = io.Copy(w, r)

	cerr := fh.Close()

	if err == nil {
		err = cerr
	}

	return err
}

// DownloadFile GETs the given URL to a local file
func (c *Client) DownloadFile(ctx context.Context, file string, u *url.URL, param *Download) error {
	var err error
	if param == nil {
		param = &DefaultDownload
	}

	rc, contentLength, err := c.Download(ctx, u, param)
	if err != nil {
		return err
	}

	return c.WriteFile(ctx, file, rc, contentLength, param.Progress, param.Writer)
}
