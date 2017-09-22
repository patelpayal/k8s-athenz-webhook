package webhook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"time"
)

// statusCodeError is an error that carries a status code
type statusCodeError struct {
	error
	code int
}

// debugTransport prints HTTP wire requests and responses with the assumption
// that these will be small and fit into memory.
type debugTransport struct {
	http.RoundTripper
	log Logger
}

// RoundTrip implements the RoundTripper interface.
func (d *debugTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	l := d.log
	l.Printf("%s %s\n", req.Method, req.URL)
	for k, v := range req.Header {
		if len(v) == 1 {
			l.Printf("\t%s: %s\n", k, v[0])
		} else {
			l.Printf("\t%s: %v\n", k, v)
		}
	}
	l.Println("end headers")
	if req.Body != nil {
		b, err := ioutil.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("could not read request body for debug logging, %v", err)
		}
		l.Println(string(b))
		req.Body = ioutil.NopCloser(bytes.NewBuffer(b))
	}

	res, err := d.RoundTripper.RoundTrip(req)
	if err != nil {
		d.log.Printf("request error: %v\n", err)
		return nil, err
	}

	l.Printf("response status: %d\n", res.StatusCode)
	for k, v := range res.Header {
		if len(v) == 1 {
			l.Printf("\t%s: %s\n", k, v[0])
		} else {
			l.Printf("\t%s: %v\n", k, v)
		}
	}
	l.Println("end headers")
	b, err := ioutil.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("could not read response body for debug: %v", err)
	}
	l.Println("response:", string(b))
	res.Body = ioutil.NopCloser(bytes.NewBuffer(b))
	return res, err
}

// authxp implements a custom transport that sets the auth header for Athenz requests.
type authxp struct {
	h string
	v string
}

func (x *authxp) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set(x.h, x.v)
	return http.DefaultTransport.RoundTrip(req)
}

func newAuthTransport(header string, token string) http.RoundTripper {
	return &authxp{h: header, v: token}
}

// client is a client to the Athenz service.
type client struct {
	endpoint string
	c        *http.Client
}

func newClient(endpoint string, timeout time.Duration, tr http.RoundTripper) *client {
	return &client{
		endpoint: endpoint,
		c: &http.Client{
			Timeout:   timeout,
			Transport: tr,
		},
	}
}

// extractMessage extracts an additional message from the Athenz response, if possible,
// for unsuccessful responses.
func extractMessage(b []byte) string {
	resourceError := struct {
		Message string `json:"message"`
	}{"no message found"}
	json.Unmarshal(b, &resourceError) // no error check needed
	return resourceError.Message
}

// request makes a GET request to the supplied URL, deserializing data into the supplied interface.
// If a validator is provided it then calls the validator with the returned body for better
// contextual messages.
func (c *client) request(u string, data interface{}, validator func(body []byte) error) error {
	res, err := c.c.Get(u)
	if err != nil {
		return fmt.Errorf("GET %s, %v", u, err)
	}
	defer res.Body.Close()

	b, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("GET %s body read error, %v", u, err)
	}
	if res.StatusCode != 200 {
		return &statusCodeError{code: res.StatusCode, error: fmt.Errorf("GET %s returned %d (%s)", u, res.StatusCode, extractMessage(b))}
	}
	if err := json.Unmarshal(b, data); err != nil {
		return fmt.Errorf("GET %s invalid JSON body %s, %v", u, b, err)
	}
	if validator != nil {
		return validator(b)
	}
	return nil
}

// authenticate make a request assuming that the transport has been configured
// to present the user's token and returns the response from Athenz.
func (c *client) authenticate() (*AthenzPrincipal, error) {
	u := fmt.Sprintf("%s/principal", c.endpoint)
	var ap AthenzPrincipal
	err := c.request(u, &ap, func(b []byte) error {
		if ap.Domain == "" || ap.Service == "" {
			return fmt.Errorf("GET %s unable to get domain and/or name from %s", u, b)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &ap, nil
}

// authorize returns true if the supplied principal has access to the resource and action.
func (c *client) authorize(principal string, check AthenzAccessCheck) (bool, error) {
	var authzResponse struct {
		Granted bool `json:"granted"`
	}
	esc := url.PathEscape
	u := fmt.Sprintf("%s/access/%s/%s?principal=%s", c.endpoint, esc(check.Action), esc(check.Resource), esc(principal))
	err := c.request(u, &authzResponse, nil)
	if err != nil {
		return false, err
	}
	return authzResponse.Granted, nil
}