package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	dockerEngineSocketPath = "/var/run/docker.sock"
	dockerEngineAPIVersion = "v1.41"
)

var (
	dockerEngineHTTPClient     *http.Client
	dockerEngineHTTPClientOnce sync.Once
)

type dockerEngineError struct {
	Message string `json:"message"`
}

type dockerPortBinding struct {
	HostIP   string `json:"HostIp,omitempty"`
	HostPort string `json:"HostPort,omitempty"`
}

type dockerRestartPolicy struct {
	Name              string `json:"Name,omitempty"`
	MaximumRetryCount int    `json:"MaximumRetryCount,omitempty"`
}

type dockerUlimit struct {
	Name string `json:"Name"`
	Soft int64  `json:"Soft"`
	Hard int64  `json:"Hard"`
}

type dockerHostConfig struct {
	Binds             []string                       `json:"Binds,omitempty"`
	PortBindings      map[string][]dockerPortBinding `json:"PortBindings,omitempty"`
	RestartPolicy     dockerRestartPolicy            `json:"RestartPolicy,omitempty"`
	CapAdd            []string                       `json:"CapAdd,omitempty"`
	CapDrop           []string                       `json:"CapDrop,omitempty"`
	SecurityOpt       []string                       `json:"SecurityOpt,omitempty"`
	PidsLimit         int64                          `json:"PidsLimit,omitempty"`
	ReadonlyRootfs    bool                           `json:"ReadonlyRootfs,omitempty"`
	Tmpfs             map[string]string              `json:"Tmpfs,omitempty"`
	Memory            int64                          `json:"Memory,omitempty"`
	MemoryReservation int64                          `json:"MemoryReservation,omitempty"`
	MemorySwap        int64                          `json:"MemorySwap,omitempty"`
	CpuShares         int64                          `json:"CpuShares,omitempty"`
	CpuPeriod         int64                          `json:"CpuPeriod,omitempty"`
	CpuQuota          int64                          `json:"CpuQuota,omitempty"`
	BlkioWeight       int64                          `json:"BlkioWeight,omitempty"`
	Ulimits           []dockerUlimit                 `json:"Ulimits,omitempty"`
}

type dockerContainerCreateRequest struct {
	Image        string              `json:"Image"`
	Env          []string            `json:"Env,omitempty"`
	Cmd          []string            `json:"Cmd,omitempty"`
	Entrypoint   []string            `json:"Entrypoint,omitempty"`
	WorkingDir   string              `json:"WorkingDir,omitempty"`
	User         string              `json:"User,omitempty"`
	Tty          bool                `json:"Tty,omitempty"`
	OpenStdin    bool                `json:"OpenStdin,omitempty"`
	AttachStdin  bool                `json:"AttachStdin,omitempty"`
	AttachStdout bool                `json:"AttachStdout,omitempty"`
	AttachStderr bool                `json:"AttachStderr,omitempty"`
	StdinOnce    bool                `json:"StdinOnce,omitempty"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	HostConfig   dockerHostConfig    `json:"HostConfig,omitempty"`
}

type dockerContainerCreateResponse struct {
	ID string `json:"Id"`
}

type dockerContainerInspectResponse struct {
	HostConfig dockerHostConfig `json:"HostConfig"`
}

type dockerPullProgress struct {
	Status string `json:"status"`
	Error  string `json:"error"`
}

type dockerAttachOptions struct {
	Stdin  bool
	Stdout bool
	Stderr bool
	Logs   bool
}

type dockerHijackedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *dockerHijackedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

type dockerImageInspectConfig struct {
	Entrypoint []string       `json:"Entrypoint"`
	Cmd        []string       `json:"Cmd"`
	WorkingDir string         `json:"WorkingDir"`
	Env        []string       `json:"Env"`
	User       string         `json:"User"`
	Volumes    map[string]any `json:"Volumes"`
}

type dockerImageInspectResponse struct {
	Config dockerImageInspectConfig `json:"Config"`
}

func sanitizeStringList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		clean := strings.TrimSpace(value)
		if clean != "" {
			out = append(out, clean)
		}
	}
	return out
}

func (d Docker) inspectImageConfigAPI(ctx context.Context, ref string) (imageConfig, error) {
	imageRef := strings.TrimSpace(ref)
	if imageRef == "" {
		return imageConfig{}, fmt.Errorf("missing image reference")
	}

	body, status, err := d.engineRequest(ctx, http.MethodGet, "/images/"+url.PathEscape(imageRef)+"/json", nil, nil)
	if err != nil {
		return imageConfig{}, err
	}
	if status < 200 || status >= 300 {
		return imageConfig{}, fmt.Errorf("failed to inspect image %s: %s", imageRef, decodeDockerAPIError(body))
	}

	var payload dockerImageInspectResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return imageConfig{}, err
	}

	cfg := imageConfig{Env: map[string]string{}}
	cfg.Entrypoint = sanitizeStringList(payload.Config.Entrypoint)
	cfg.Cmd = sanitizeStringList(payload.Config.Cmd)
	cfg.Workdir = strings.TrimSpace(payload.Config.WorkingDir)
	cfg.User = strings.TrimSpace(payload.Config.User)

	if len(payload.Config.Volumes) > 0 {
		cfg.Volumes = make([]string, 0, len(payload.Config.Volumes))
		for volumePath := range payload.Config.Volumes {
			clean := strings.TrimSpace(volumePath)
			if clean != "" && clean != "/" {
				cfg.Volumes = append(cfg.Volumes, clean)
			}
		}
		sort.Strings(cfg.Volumes)
	}

	for _, kv := range sanitizeStringList(payload.Config.Env) {
		if k, v, ok := strings.Cut(kv, "="); ok {
			cfg.Env[strings.TrimSpace(k)] = v
		}
	}

	return cfg, nil
}

func dockerEngineClient() *http.Client {
	dockerEngineHTTPClientOnce.Do(func() {
		transport := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", dockerEngineSocketPath)
			},
			DisableCompression: true,
		}
		dockerEngineHTTPClient = &http.Client{
			Transport: transport,
			Timeout:   0,
		}
	})
	return dockerEngineHTTPClient
}

func dockerAPIPath(rawPath string, query url.Values) string {
	path := "/" + dockerEngineAPIVersion + rawPath
	if query == nil || len(query) == 0 {
		return path
	}
	return path + "?" + query.Encode()
}

func decodeDockerAPIError(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return "docker engine request failed"
	}
	var payload dockerEngineError
	if err := json.Unmarshal(body, &payload); err == nil && strings.TrimSpace(payload.Message) != "" {
		return strings.TrimSpace(payload.Message)
	}
	return trimmed
}

func (d Docker) engineRequest(ctx context.Context, method, rawPath string, query url.Values, body any) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+dockerAPIPath(rawPath, query), reader)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := dockerEngineClient().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, resp.StatusCode, readErr
	}
	return respBody, resp.StatusCode, nil
}

func (d Docker) attachContainerAPI(ctx context.Context, name string, opts dockerAttachOptions) (io.ReadWriteCloser, error) {
	dockerName := dockerContainerName(name)
	if dockerName == "" {
		return nil, fmt.Errorf("missing container name")
	}
	if !opts.Stdin && !opts.Stdout && !opts.Stderr {
		return nil, fmt.Errorf("no attach streams requested")
	}

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", dockerEngineSocketPath)
	if err != nil {
		return nil, err
	}

	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	query := url.Values{}
	query.Set("stream", "1")
	if opts.Stdin {
		query.Set("stdin", "1")
	}
	if opts.Stdout {
		query.Set("stdout", "1")
	}
	if opts.Stderr {
		query.Set("stderr", "1")
	}
	if opts.Logs {
		query.Set("logs", "1")
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"http://docker"+dockerAPIPath("/containers/"+url.PathEscape(dockerName)+"/attach", query),
		nil,
	)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")

	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		detail := ""
		if resp.Body != nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			detail = decodeDockerAPIError(body)
		}
		_ = conn.Close()
		if detail == "" {
			detail = resp.Status
		}
		return nil, fmt.Errorf("failed to attach container %s: %s", dockerName, detail)
	}

	_ = conn.SetDeadline(time.Time{})
	return &dockerHijackedConn{Conn: conn, reader: reader}, nil
}

func splitDockerImageRef(ref string) (string, string) {
	clean := strings.TrimSpace(ref)
	if clean == "" {
		return "", ""
	}
	if at := strings.Index(clean, "@"); at > 0 {
		return clean, ""
	}
	lastSlash := strings.LastIndex(clean, "/")
	lastColon := strings.LastIndex(clean, ":")
	if lastColon > lastSlash {
		return clean[:lastColon], clean[lastColon+1:]
	}
	return clean, "latest"
}

func (d Docker) pullImageAPI(ctx context.Context, ref string) error {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		err := d.pullImageAPIOnce(ctx, ref)
		if err == nil {
			return nil
		}
		lastErr = err
		if ctx.Err() != nil || !isTransientDockerPullError(err) || attempt == 3 {
			break
		}
		delay := time.Duration(attempt*attempt) * time.Second
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return lastErr
}

func (d Docker) ensureImageAvailableAPI(ctx context.Context, ref string) error {
	imageRef := strings.TrimSpace(ref)
	if imageRef == "" {
		return fmt.Errorf("missing image reference")
	}
	if d.imageExistsLocally(ctx, imageRef) {
		return nil
	}
	if err := d.pullImageAPI(ctx, imageRef); err != nil {
		if isTransientDockerPullError(err) && d.imageExistsLocally(ctx, imageRef) {
			fmt.Printf("[docker] pull failed for %s with transient error, using cached image: %v\n", imageRef, err)
			return nil
		}
		return err
	}
	return nil
}

func (d Docker) pullImageAPIOnce(ctx context.Context, ref string) error {
	image, tag := splitDockerImageRef(ref)
	if image == "" {
		return fmt.Errorf("missing image reference")
	}
	query := url.Values{}
	query.Set("fromImage", image)
	if tag != "" {
		query.Set("tag", tag)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker"+dockerAPIPath("/images/create", query), nil)
	if err != nil {
		return err
	}
	resp, err := dockerEngineClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to pull image %s: %s", ref, decodeDockerAPIError(body))
	}

	dec := json.NewDecoder(resp.Body)
	for {
		var item dockerPullProgress
		if err := dec.Decode(&item); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		if strings.TrimSpace(item.Error) != "" {
			return fmt.Errorf("failed to pull image %s: %s", ref, strings.TrimSpace(item.Error))
		}
	}
	return nil
}

func isTransientDockerPullError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	nonTransient := []string{
		"manifest unknown",
		"not found",
		"pull access denied",
		"repository does not exist",
		"requested access to the resource is denied",
		"unauthorized",
		"authentication required",
		"invalid reference format",
	}
	for _, marker := range nonTransient {
		if strings.Contains(msg, marker) {
			return false
		}
	}
	transient := []string{
		"tls: bad record mac",
		"unexpected eof",
		"connection reset",
		"connection refused",
		"connection timed out",
		"i/o timeout",
		"net/http",
		"httpreadseeker",
		"failed to copy",
		"failed to do request",
		"tls handshake timeout",
		"temporary failure",
		"503",
		"502",
		"504",
		"too many requests",
	}
	for _, marker := range transient {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func (d Docker) removeContainerAPI(ctx context.Context, name string, force bool) error {
	query := url.Values{}
	if force {
		query.Set("force", "1")
	}
	body, status, err := d.engineRequest(ctx, http.MethodDelete, "/containers/"+url.PathEscape(name), query, nil)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return nil
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("failed to remove container %s: %s", name, decodeDockerAPIError(body))
	}
	return nil
}

func (d Docker) createContainerAPI(ctx context.Context, name string, req dockerContainerCreateRequest) (string, error) {
	query := url.Values{}
	query.Set("name", name)
	body, status, err := d.engineRequest(ctx, http.MethodPost, "/containers/create", query, req)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("failed to create container %s: %s", name, decodeDockerAPIError(body))
	}

	var created dockerContainerCreateResponse
	if err := json.Unmarshal(body, &created); err != nil {
		return "", err
	}
	if strings.TrimSpace(created.ID) == "" {
		return "", fmt.Errorf("docker engine did not return a container id")
	}
	return created.ID, nil
}

func (d Docker) inspectContainerAPI(ctx context.Context, name string) (dockerContainerInspectResponse, error) {
	body, status, err := d.engineRequest(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil, nil)
	if err != nil {
		return dockerContainerInspectResponse{}, err
	}
	if status < 200 || status >= 300 {
		return dockerContainerInspectResponse{}, fmt.Errorf("failed to inspect container %s: %s", name, decodeDockerAPIError(body))
	}

	var payload dockerContainerInspectResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return dockerContainerInspectResponse{}, err
	}
	return payload, nil
}

func (d Docker) startContainerAPI(ctx context.Context, name string) error {
	body, status, err := d.engineRequest(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/start", nil, nil)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("failed to start container %s: %s", name, decodeDockerAPIError(body))
	}
	return nil
}

func (d Docker) runContainerAPI(ctx context.Context, name string, req dockerContainerCreateRequest) error {
	removeCtx, removeCancel := context.WithTimeout(ctx, 15*time.Second)
	_ = d.removeContainerAPI(removeCtx, name, true)
	removeCancel()

	pullCtx, pullCancel := context.WithTimeout(ctx, 10*time.Minute)
	if err := d.ensureImageAvailableAPI(pullCtx, req.Image); err != nil {
		pullCancel()
		return err
	}
	pullCancel()

	if _, err := d.createContainerAPI(ctx, name, req); err != nil {
		return err
	}
	return d.startContainerAPI(ctx, name)
}
