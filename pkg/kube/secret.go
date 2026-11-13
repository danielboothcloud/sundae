// Package kube is a minimal Kubernetes API client for the one thing Sundae's
// Helm chart needs from inside the cluster: creating a Secret with a random
// value once, and leaving it alone afterwards.
package kube

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ServiceAccountDir is where Kubernetes mounts a pod's service account
// token, CA certificate and namespace.
const ServiceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// Client talks to the Kubernetes API server.
type Client struct {
	// BaseURL is the API server, e.g. https://10.0.0.1:443.
	BaseURL string
	// Token is the bearer token sent with every request.
	Token string
	// HTTP is the client used for requests.
	HTTP *http.Client
}

// InCluster returns a client for the API server of the cluster the process
// runs in, authenticated as the pod's service account, and the pod's
// namespace.
func InCluster() (*Client, string, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, "", errors.New("not running in a Kubernetes pod: KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT are unset")
	}
	token, err := os.ReadFile(filepath.Join(ServiceAccountDir, "token"))
	if err != nil {
		return nil, "", fmt.Errorf("service account token: %w", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(ServiceAccountDir, "ca.crt"))
	if err != nil {
		return nil, "", fmt.Errorf("service account CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, "", errors.New("service account CA: no certificates found")
	}
	ns, err := os.ReadFile(filepath.Join(ServiceAccountDir, "namespace"))
	if err != nil {
		return nil, "", fmt.Errorf("service account namespace: %w", err)
	}
	return &Client{
		BaseURL: "https://" + net.JoinHostPort(host, port),
		Token:   strings.TrimSpace(string(token)),
		HTTP: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		},
	}, strings.TrimSpace(string(ns)), nil
}

// Secret is the part of a core/v1 Secret this package reads and writes.
type Secret struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   ObjectMeta        `json:"metadata"`
	Type       string            `json:"type,omitempty"`
	Data       map[string][]byte `json:"data,omitempty"`
}

// ObjectMeta is the part of an object's metadata this package uses.
type ObjectMeta struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// RandomBase64 returns n random bytes, base64-encoded (like
// `openssl rand -base64 n`).
func RandomBase64(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return []byte(base64.StdEncoding.EncodeToString(b)), nil
}

// EnsureSecret makes sure the Secret namespace/name exists with a non-empty
// key. When the Secret does not exist it is created from template (its
// metadata name and namespace are set here) with key set to value(). An
// existing Secret is never changed: when it lacks key, EnsureSecret fails.
// It reports whether it created the Secret.
func (c *Client) EnsureSecret(ctx context.Context, namespace, name, key string, template Secret, value func() ([]byte, error)) (bool, error) {
	path := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/secrets"
	check := func() (bool, error) {
		var s Secret
		found, err := c.do(ctx, http.MethodGet, path+"/"+url.PathEscape(name), nil, &s)
		if err != nil || !found {
			return found, err
		}
		if len(s.Data[key]) == 0 {
			return true, fmt.Errorf("secret %s/%s exists without key %q; add it or delete the secret", namespace, name, key)
		}
		return true, nil
	}
	if found, err := check(); found || err != nil {
		return false, err
	}

	v, err := value()
	if err != nil {
		return false, fmt.Errorf("generate value: %w", err)
	}
	s := template
	s.APIVersion, s.Kind = "v1", "Secret"
	s.Metadata.Name, s.Metadata.Namespace = name, namespace
	if s.Type == "" {
		s.Type = "Opaque"
	}
	s.Data = map[string][]byte{key: v}
	body, err := json.Marshal(s)
	if err != nil {
		return false, err
	}
	created, err := c.do(ctx, http.MethodPost, path, body, nil)
	if err != nil {
		return false, err
	}
	if created {
		return true, nil
	}
	// Created by someone else in the meantime: check theirs.
	found, err := check()
	if err == nil && !found {
		err = fmt.Errorf("secret %s/%s: create reported a conflict, but it does not exist", namespace, name)
	}
	return false, err
}

// do sends one request. It reports false, without an error, for 404 on GET
// and 409 on POST, and decodes a successful response into out when set.
func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close() //nolint:errcheck
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, err
	}
	switch {
	case method == http.MethodGet && resp.StatusCode == http.StatusNotFound,
		method == http.MethodPost && resp.StatusCode == http.StatusConflict:
		return false, nil
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return false, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, apiMessage(data))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return false, fmt.Errorf("%s %s: decode response: %w", method, path, err)
		}
	}
	return true, nil
}

// apiMessage returns the message of a Kubernetes Status response, else the
// body itself.
func apiMessage(body []byte) string {
	var st struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &st) == nil && st.Message != "" {
		return st.Message
	}
	return strings.TrimSpace(string(body))
}
