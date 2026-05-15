package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/ulikunitz/xz"
)

const (
	NodeVersion       = "1.2.5"
	DefaultConfigPath = "/var/lib/node/config.yml"
	DefaultDataRoot   = "/var/lib/node"
	DefaultAPIHost    = "0.0.0.0"
	DefaultAPIPort    = 8080
	DefaultSFTPPort   = 2022
	NodeInfoFileName  = "node-information.json"
	NodeUpdateMaxBody = 20 * 1024 * 1024
)

type Config struct {
	Raw map[string]any
}

type NodeInformation struct {
	NodeVersion      string `json:"node-version"`
	NodeArchitecture string `json:"node-architecture"`
	NodeAutoUpdate   bool   `json:"node-auto-update"`
	UpdatedAt        string `json:"updated-at,omitempty"`
}

type nodeUpdateFilePayload struct {
	Path          string `json:"path"`
	ContentBase64 string `json:"contentBase64"`
}

type nodeUpdateInstallRequest struct {
	Version string                  `json:"version"`
	Files   []nodeUpdateFilePayload `json:"files"`
}

type nodeUpdateAutoRequest struct {
	Enabled bool `json:"enabled"`
}

func (c Config) Bool(path ...string) bool {
	v := c.Any(path...)
	b, _ := v.(bool)
	return b
}
func (c Config) String(path ...string) string {
	v := c.Any(path...)
	switch x := v.(type) {
	case string:
		return x
	default:
		return ""
	}
}
func (c Config) Int(path ...string) int {
	v := c.Any(path...)
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(x))
		return n
	default:
		return 0
	}
}
func (c Config) Any(path ...string) any {
	cur := any(c.Raw)
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

func parseYAMLSubset(src string) (map[string]any, error) {
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	root := map[string]any{}
	type frame struct {
		indent int
		obj    map[string]any
	}
	stack := []frame{{indent: -1, obj: root}}

	for lineNum, raw := range lines {
		line := strings.ReplaceAll(raw, "\t", "  ")
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		for len(stack) > 0 && indent <= stack[len(stack)-1].indent {
			stack = stack[:len(stack)-1]
		}
		if len(stack) == 0 {
			return nil, fmt.Errorf("yaml parse: invalid indent at line %d", lineNum+1)
		}
		cur := stack[len(stack)-1].obj

		parts := strings.SplitN(strings.TrimSpace(line), ":", 2)
		if len(parts) != 2 {
			fmt.Fprintf(os.Stderr, "[warn] yaml parse: malformed line %d (missing colon): %s\n", lineNum+1, trim)
			continue
		}
		key := strings.TrimSpace(parts[0])
		valRaw := strings.TrimSpace(parts[1])

		if valRaw == "" {
			obj := map[string]any{}
			cur[key] = obj
			stack = append(stack, frame{indent: indent, obj: obj})
			continue
		}

		val := any(valRaw)
		if (strings.HasPrefix(valRaw, `"`) && strings.HasSuffix(valRaw, `"`)) ||
			(strings.HasPrefix(valRaw, `'`) && strings.HasSuffix(valRaw, `'`)) {
			val = valRaw[1 : len(valRaw)-1]
		} else if strings.EqualFold(valRaw, "true") || strings.EqualFold(valRaw, "false") {
			val = strings.EqualFold(valRaw, "true")
		} else if n, err := strconv.ParseInt(valRaw, 10, 64); err == nil {
			val = int(n)
		} else if f, err := strconv.ParseFloat(valRaw, 64); err == nil {
			val = f
		}
		cur[key] = val
	}
	return root, nil
}

func mustReadFile(p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[fatal] read %s: %v\n", p, err)
		os.Exit(1)
	}
	return b
}

func ensureDir(p string) error {
	return os.MkdirAll(p, 0o755)
}

func anyToInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case float32:
		return int(x)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(x))
		return n
	default:
		return 0
	}
}

func anyToInt64(v any) int64 {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int64:
		return x
	case int32:
		return int64(x)
	case float64:
		return int64(x)
	case float32:
		return int64(x)
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n
	default:
		return 0
	}
}

func anyToFloat64(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case string:
		n, _ := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return n
	default:
		return 0
	}
}

func openWriteNoFollow(path string, perm os.FileMode) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, perm)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("refusing to write through symlink: %s", path)
		}
		return nil, err
	}
	return f, nil
}

func writeFileNoFollow(path string, data []byte, perm os.FileMode) error {
	f, err := openWriteNoFollow(path, perm)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

func normalizeNodeVersionTag(version string) string {
	clean := strings.TrimSpace(version)
	if clean == "" {
		return "v" + NodeVersion
	}
	if strings.HasPrefix(strings.ToLower(clean), "v") {
		return "v" + strings.TrimPrefix(strings.TrimPrefix(clean, "v"), "V")
	}
	return "v" + clean
}

func validNodeUpdateVersion(version string) bool {
	return regexp.MustCompile(`^v?\d+\.\d+\.\d+([\-+][A-Za-z0-9_.-]+)?$`).MatchString(strings.TrimSpace(version))
}

func allowedNodeUpdateFile(rel string) bool {
	raw := strings.TrimSpace(rel)
	if strings.Contains(raw, "/") || strings.Contains(raw, "\\") {
		return false
	}
	clean := filepath.Clean(raw)
	if clean == "." || clean == "" || strings.Contains(clean, "/") || strings.Contains(clean, string(filepath.Separator)) {
		return false
	}
	if clean == "go.mod" || clean == "go.sum" {
		return true
	}
	return regexp.MustCompile(`^[A-Za-z0-9_.-]+\.go$`).MatchString(clean)
}

func validateNodeUpdateFileContent(rel string, data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("%s is empty", rel)
	}
	if len(data) > 8*1024*1024 {
		return fmt.Errorf("%s is too large", rel)
	}
	if strings.HasSuffix(rel, ".go") && !bytes.HasPrefix(bytes.TrimSpace(data), []byte("package main")) {
		return fmt.Errorf("%s is not a package main Go file", rel)
	}
	if rel == "go.mod" && !bytes.HasPrefix(bytes.TrimSpace(data), []byte("module ")) {
		return fmt.Errorf("go.mod is invalid")
	}
	return nil
}

func copyRegularFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return nil
	}
	if err := ensureDir(filepath.Dir(dst)); err != nil {
		return err
	}
	out, err := openWriteNoFollow(dst, st.Mode().Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func sanitizeName(raw string) string {
	s := strings.TrimSpace(raw)
	s = regexp.MustCompile(`\s+`).ReplaceAllString(s, "-")
	s = regexp.MustCompile(`[^\w\-_.]+`).ReplaceAllString(s, "")
	s = strings.Trim(s, "-_.")
	for len(s) > 0 && !((s[0] >= 'a' && s[0] <= 'z') || (s[0] >= 'A' && s[0] <= 'Z') || (s[0] >= '0' && s[0] <= '9')) {
		s = s[1:]
	}
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

func isValidContainerName(name string) bool {
	if name == "" || len(name) > 120 {
		return false
	}
	if !((name[0] >= 'a' && name[0] <= 'z') || (name[0] >= 'A' && name[0] <= 'Z') || (name[0] >= '0' && name[0] <= '9')) {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '.' || c == '-') {
			return false
		}
	}
	return true
}

func isDockerNativeContainerName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	if len(name) < 2 {
		return false
	}
	if !((name[0] >= 'a' && name[0] <= 'z') || (name[0] >= 'A' && name[0] <= 'Z') || (name[0] >= '0' && name[0] <= '9')) {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '.' || c == '-') {
			return false
		}
	}
	return true
}

func dockerContainerName(name string) string {
	clean := strings.TrimSpace(name)
	if isDockerNativeContainerName(clean) {
		return clean
	}
	safe := sanitizeName(clean)
	if safe == "" {
		return clean
	}
	sum := sha256.Sum256([]byte(clean))
	hash := hex.EncodeToString(sum[:])[:12]
	if len(safe) > 64 {
		safe = safe[:64]
	}
	return "adpanel-" + hash + "-" + safe
}

func dockerContainerSpecName(spec string) string {
	if left, right, ok := strings.Cut(spec, ":"); ok && left != "" && !strings.Contains(left, "/") {
		return dockerContainerName(left) + ":" + right
	}
	return spec
}

func rewriteDockerCLIArgs(args []string) []string {
	if len(args) == 0 {
		return args
	}
	out := append([]string(nil), args...)
	rewriteLast := func() {
		if len(out) > 1 {
			out[len(out)-1] = dockerContainerName(out[len(out)-1])
		}
	}

	switch out[0] {
	case "logs", "diff", "stop", "kill", "restart", "stats", "update":
		rewriteLast()
	case "inspect":
		rewriteLast()
	case "rm":
		for i := 1; i < len(out); i++ {
			if strings.HasPrefix(out[i], "-") {
				continue
			}
			out[i] = dockerContainerName(out[i])
		}
	case "exec":
		for i := 1; i < len(out); i++ {
			if strings.HasPrefix(out[i], "-") {
				continue
			}
			out[i] = dockerContainerName(out[i])
			break
		}
	case "cp":
		for i := 1; i < len(out); i++ {
			out[i] = dockerContainerSpecName(out[i])
		}
	case "run":
		for i := 1; i < len(out)-1; i++ {
			if out[i] == "--name" {
				out[i+1] = dockerContainerName(out[i+1])
				i++
			}
		}
	}
	return out
}

func safeJoin(base, rel string) (string, bool) {
	r := strings.TrimSpace(rel)
	if r == "" {
		return filepath.Clean(base), true
	}
	if filepath.IsAbs(r) {
		r = strings.TrimLeft(r, string(os.PathSeparator))
	}
	target := filepath.Join(base, r)
	baseC := filepath.Clean(base)
	targetC := filepath.Clean(target)

	relToBase, err := filepath.Rel(baseC, targetC)
	if err != nil {
		return "", false
	}
	if relToBase == "." {
		return targetC, true
	}
	if strings.HasPrefix(relToBase, ".."+string(os.PathSeparator)) || relToBase == ".." || filepath.IsAbs(relToBase) {
		return "", false
	}
	return targetC, true
}

func isUnder(base, target string) bool {
	baseC := filepath.Clean(base)
	targetC := filepath.Clean(target)
	rel, err := filepath.Rel(baseC, targetC)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && rel != ".." && !filepath.IsAbs(rel))
}

func ensureResolvedUnder(base, p string) error {
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return err
	}
	baseC := filepath.Clean(base)
	if !isUnder(baseC, resolved) && baseC != resolved {
		return fmt.Errorf("symlink escape blocked")
	}
	return nil
}

func panelSafeCaps() []string {
	return []string{
		"--cap-add=CHOWN",
		"--cap-add=DAC_OVERRIDE",
		"--cap-add=FOWNER",
		"--cap-add=SETUID",
		"--cap-add=SETGID",
		"--cap-add=KILL",
		"--cap-add=NET_BIND_SERVICE",
		"--cap-add=NET_RAW",
		"--cap-add=SYS_CHROOT",
		"--cap-add=AUDIT_WRITE",
	}
}

func containerSecurityArgs(uid, gid int, readOnly bool) []string {
	args := []string{"--cap-drop=ALL"}
	args = append(args, panelSafeCaps()...)
	args = append(args,
		"--security-opt=no-new-privileges",
		"--pids-limit=512",
		fmt.Sprintf("--user=%d:%d", uid, gid),
	)
	if readOnly {
		args = append(args,
			"--read-only",
			"--tmpfs", "/tmp:rw,noexec,nosuid,size=256m",
			"--tmpfs", "/var/tmp:rw,noexec,nosuid,size=64m",
			"--tmpfs", "/run:rw,noexec,nosuid,size=32m",
		)
	}
	return args
}

func timingSafeEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func jsonWrite(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(w http.ResponseWriter, r *http.Request, limitBytes int64, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, limitBytes)
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

type Docker struct {
	debug bool
}

func (d Docker) runCollect(ctx context.Context, args ...string) (string, string, int, error) {
	args = rewriteDockerCLIArgs(args)
	cmd := exec.CommandContext(ctx, "docker", args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	exit := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		} else {
			exit = -1
		}
		return out.String(), errb.String(), exit, err
	}
	return out.String(), errb.String(), 0, nil
}

func (d Docker) containerExists(ctx context.Context, name string) bool {
	_, _, _, err := d.runCollect(ctx, "inspect", name)
	return err == nil
}

func (d Docker) containerState(ctx context.Context, name string) string {
	out, _, _, err := d.runCollect(ctx, "inspect", "-f", "{{.State.Status}}", name)
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(out))
}

func (d Docker) kill(ctx context.Context, name string) {
	_, _, _, _ = d.runCollect(ctx, "kill", name)
}

func (d Docker) rmForce(ctx context.Context, name string) {
	d.kill(ctx, name)
	_, _, _, _ = d.runCollect(ctx, "rm", "-f", "-v", name)
}

func (d Docker) pull(ctx context.Context, ref string) {
	if err := d.pullImageAPI(ctx, ref); err != nil {
		_, _, _, _ = d.runCollect(ctx, "pull", ref)
	}
}

func (d Docker) imageExistsLocally(ctx context.Context, ref string) bool {
	_, _, exitCode, _ := d.runCollect(ctx, "image", "inspect", ref)
	return exitCode == 0
}

type imageConfig struct {
	Volumes    []string
	Workdir    string
	User       string
	Env        map[string]string
	Entrypoint []string
	Cmd        []string
}

func (d Docker) inspectImageConfig(ctx context.Context, ref string) imageConfig {
	if apiCfg, err := d.inspectImageConfigAPI(ctx, ref); err == nil {
		return apiCfg
	}

	cfg := imageConfig{Env: map[string]string{}}

	out, _, code, err := d.runCollect(ctx, "inspect", "--format",
		`{{json .Config.Volumes}}||{{.Config.WorkingDir}}||{{.Config.User}}||{{json .Config.Env}}||{{json .Config.Entrypoint}}||{{json .Config.Cmd}}`, ref)
	if err != nil || code != 0 {
		return cfg
	}

	parts := strings.SplitN(strings.TrimSpace(out), "||", 6)
	if len(parts) < 3 {
		return cfg
	}

	volJson := strings.TrimSpace(parts[0])
	if volJson != "" && volJson != "null" && volJson != "{}" {
		var vols map[string]any
		if jsonErr := json.Unmarshal([]byte(volJson), &vols); jsonErr == nil {
			for p := range vols {
				p = strings.TrimSpace(p)
				if p != "" && p != "/" {
					cfg.Volumes = append(cfg.Volumes, p)
				}
			}
		}
	}

	cfg.Workdir = strings.TrimSpace(parts[1])
	cfg.User = strings.TrimSpace(parts[2])

	if len(parts) >= 4 {
		envJson := strings.TrimSpace(parts[3])
		if envJson != "" && envJson != "null" {
			var envArr []string
			if jsonErr := json.Unmarshal([]byte(envJson), &envArr); jsonErr == nil {
				for _, kv := range envArr {
					if k, v, ok := strings.Cut(kv, "="); ok {
						cfg.Env[k] = v
					}
				}
			}
		}
	}

	if len(parts) >= 5 {
		cfg.Entrypoint = parseImageConfigStringList(parts[4])
	}

	if len(parts) >= 6 {
		cfg.Cmd = parseImageConfigStringList(parts[5])
	}

	return cfg
}

func parseImageConfigStringList(raw string) []string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var list []string
	if err := json.Unmarshal([]byte(trimmed), &list); err != nil {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		clean := strings.TrimSpace(item)
		if clean != "" {
			out = append(out, clean)
		}
	}
	return out
}

func runtimeArgNeedsQuoting(arg string) bool {
	if arg == "" {
		return true
	}
	for _, r := range arg {
		switch r {
		case ' ', '\t', '\n', '\r', '"', '\\':
			return true
		}
	}
	return false
}

func quoteRuntimeArg(arg string) string {
	if !runtimeArgNeedsQuoting(arg) {
		return arg
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range arg {
		if r == '"' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

func runtimeCommandFromArgs(args []string) string {
	if len(args) == 0 {
		return ""
	}
	encoded := make([]string, 0, len(args))
	for _, arg := range args {
		clean := strings.TrimSpace(arg)
		if clean == "" {
			continue
		}
		encoded = append(encoded, quoteRuntimeArg(clean))
	}
	return strings.TrimSpace(strings.Join(encoded, " "))
}

func trimRuntimeProcessArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		clean := strings.TrimSpace(arg)
		if clean != "" {
			out = append(out, clean)
		}
	}
	return out
}

func isRuntimeShellName(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "sh", "bash", "ash", "dash", "zsh", "ksh":
		return true
	default:
		return false
	}
}

func shellEntrypointInfo(entrypoint []string) (bool, int) {
	if len(entrypoint) < 2 {
		return false, 0
	}

	execIndex := 0
	flagIndex := 1
	cmdIndex := 2
	execName := strings.ToLower(strings.TrimSpace(filepath.Base(entrypoint[execIndex])))

	if execName == "env" && len(entrypoint) >= 3 {
		execIndex = 1
		flagIndex = 2
		cmdIndex = 3
		execName = strings.ToLower(strings.TrimSpace(filepath.Base(entrypoint[execIndex])))
	}

	if !isRuntimeShellName(execName) || len(entrypoint) <= flagIndex {
		return false, 0
	}

	flags := strings.TrimSpace(entrypoint[flagIndex])
	if !strings.HasPrefix(flags, "-") || !strings.Contains(flags, "c") {
		return false, 0
	}

	return true, cmdIndex
}

func runtimeCommandFromImageConfig(cfg imageConfig) string {
	entrypoint := trimRuntimeProcessArgs(cfg.Entrypoint)
	cmd := trimRuntimeProcessArgs(cfg.Cmd)

	if isShell, cmdIndex := shellEntrypointInfo(entrypoint); isShell {
		if len(cmd) > 0 {
			// Docker treats Cmd as a single shell command string when Entrypoint is a shell wrapper.
			return strings.TrimSpace(strings.Join(cmd, " "))
		}
		if len(entrypoint) > cmdIndex {
			return strings.TrimSpace(strings.Join(entrypoint[cmdIndex:], " "))
		}
		return ""
	}

	args := make([]string, 0, len(entrypoint)+len(cmd))
	args = append(args, entrypoint...)
	args = append(args, cmd...)
	return runtimeCommandFromArgs(args)
}

func getDataPathsFromEnv(env map[string]string) []string {
	dataKeys := []string{
		"SERVER_DIR", "SERVERDIR", "SRV_DIR", "GAME_DIR", "GAMEDIR",
		"DATA_DIR", "DATADIR", "INSTALL_DIR", "INSTALLDIR",
		"STEAMAPPDIR", "HOMEDIR", "HOME_DIR",
		"APP_DIR", "APPDIR", "WORKDIR", "WORK_DIR",
	}
	seen := map[string]bool{}
	var paths []string
	for _, key := range dataKeys {
		if val, ok := env[key]; ok {
			val = strings.TrimSpace(val)
			if val != "" && val != "/" && strings.HasPrefix(val, "/") && !seen[val] {
				paths = append(paths, val)
				seen[val] = true
			}
		}
	}
	return paths
}

func (d Docker) getImageVolumePaths(ctx context.Context, ref string) []string {
	return d.inspectImageConfig(ctx, ref).Volumes
}

func ensureVolumeWritable(serverDir string) {
	if err := os.Chmod(serverDir, 0777); err != nil {
		fmt.Printf("[volumes] warning: chmod 0777 %s failed: %v\n", serverDir, err)
	}
	_ = filepath.Walk(serverDir, func(child string, info os.FileInfo, err error) error {
		if err != nil || child == serverDir || info == nil {
			return nil
		}
		if info.IsDir() {
			_ = os.Chmod(child, 0777)
			return nil
		}
		mode := os.FileMode(0666)
		if info.Mode().Perm()&0111 != 0 {
			mode = 0777
		}
		_ = os.Chmod(child, mode)
		return nil
	})
}

func (a *Agent) recoverMisplacedFiles(name, serverDir string) {
	for _, waitTime := range []time.Duration{15 * time.Second, 45 * time.Second} {
		time.Sleep(waitTime)

		entries, err := os.ReadDir(serverDir)
		if err != nil {
			return
		}
		realFileCount := 0
		realRegularFiles := 0
		for _, e := range entries {
			if e.Name() == ".adpanel" || e.Name() == ".adpanel_meta" {
				continue
			}
			realFileCount++
			if !e.IsDir() {
				realRegularFiles++
			}
		}
		if realRegularFiles > 0 || realFileCount > 1 {
			fmt.Printf("[file-recovery] %s: server directory has %d entries (%d regular files), volume mount is working\n", name, realFileCount, realRegularFiles)
			return
		}
		if realFileCount > 0 && waitTime < 45*time.Second {
			fmt.Printf("[file-recovery] %s: server directory has only %d directory(ies) and no regular files — waiting longer before recovery\n", name, realFileCount)
			continue
		}
	}

	fmt.Printf("[file-recovery] %s: server directory is EMPTY after 60s — attempting file recovery\n", name)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	statusOut, _, _, _ := a.docker.runCollect(ctx, "inspect", "--format", "{{.State.Running}}", name)
	if strings.TrimSpace(statusOut) != "true" {
		fmt.Printf("[file-recovery] %s: container not running, skipping recovery\n", name)
		return
	}

	diffOut, _, _, diffErr := a.docker.runCollect(ctx, "diff", name)
	if diffErr != nil || strings.TrimSpace(diffOut) == "" {
		fmt.Printf("[file-recovery] %s: docker diff returned no changes\n", name)
		return
	}

	dataRoots := map[string]int{}
	for _, line := range strings.Split(diffOut, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			continue
		}
		action := parts[0]
		filePath := parts[1]

		if action != "A" {
			continue
		}

		skipPrefixes := []string{"/tmp/", "/var/", "/proc/", "/sys/",
			"/dev/", "/etc/", "/root/", "/run/", "/opt/", "/usr/", "/home/"}
		skip := false
		for _, prefix := range skipPrefixes {
			if strings.HasPrefix(filePath, prefix) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}

		clean := filepath.Clean(filePath)
		segments := strings.Split(strings.TrimPrefix(clean, "/"), "/")
		if len(segments) == 0 {
			continue
		}

		root := "/" + segments[0]
		if len(segments) >= 2 {
			root = "/" + segments[0] + "/" + segments[1]
		}
		dataRoots[root]++
	}

	if len(dataRoots) == 0 {
		fmt.Printf("[file-recovery] %s: no significant file changes found in container\n", name)
		return
	}

	bestRoot := ""
	bestCount := 0
	for root, count := range dataRoots {
		if count > bestCount {
			bestRoot = root
			bestCount = count
		}
	}

	fmt.Printf("[file-recovery] %s: detected data root '%s' with %d new files — copying to server directory\n", name, bestRoot, bestCount)

	cpCtx, cpCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cpCancel()

	srcPath := name + ":" + bestRoot + "/."
	_, cpErr, cpCode, cpGoErr := a.docker.runCollect(cpCtx, "cp", srcPath, serverDir+"/")
	if cpGoErr != nil || cpCode != 0 {
		srcPath = name + ":" + bestRoot
		_, cpErr, cpCode, cpGoErr = a.docker.runCollect(cpCtx, "cp", srcPath, serverDir+"/")
		if cpGoErr != nil || cpCode != 0 {
			fmt.Printf("[file-recovery] %s: docker cp failed: %s\n", name, cpErr)
			return
		}
	}

	ensureVolumeWritable(serverDir)

	correctVolume := fmt.Sprintf("%s:%s", serverDir, bestRoot)
	_ = a.modifyMeta(serverDir, func(meta map[string]any) {
		runtimeObj, _ := meta["runtime"].(map[string]any)
		if runtimeObj == nil {
			runtimeObj = make(map[string]any)
		}

		runtimeObj["volumes"] = []any{correctVolume}
		meta["runtime"] = runtimeObj

		if sc, ok := meta["startupCommand"].(string); ok && sc != "" {
			fmt.Printf("[file-recovery] %s: startup command exists, runtime volumes updated to %s\n", name, correctVolume)
		}
	})

	fmt.Printf("[file-recovery] %s: recovered %d files from container path '%s' — volume config updated for future starts\n", name, bestCount, bestRoot)

	newEntries, _ := os.ReadDir(serverDir)
	newCount := 0
	for _, e := range newEntries {
		if e.Name() != ".adpanel" && e.Name() != ".adpanel_meta" {
			newCount++
		}
	}
	fmt.Printf("[file-recovery] %s: server directory now has %d files\n", name, newCount)
}

func (d Docker) extractImageFiles(ctx context.Context, imageRef, destDir, extractPath string) error {
	if extractPath == "" {
		extractPath = "/"
	}

	extractPath = strings.TrimSuffix(extractPath, "/")
	if extractPath == "" {
		extractPath = "/"
	}

	tempName := "adpanel-extract-" + fmt.Sprintf("%d", time.Now().UnixNano())

	_, errStr, code, err := d.runCollect(ctx, "create", "--name", tempName, imageRef, "true")
	if err != nil || code != 0 {
		return fmt.Errorf("failed to create temp container: %s", strings.TrimSpace(errStr))
	}

	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		d.rmForce(cleanupCtx, tempName)
	}()

	srcPath := tempName + ":" + extractPath
	if extractPath != "/" {
		srcPath = srcPath + "/."
	}

	_, errStr, code, err = d.runCollect(ctx, "cp", srcPath, destDir+"/")
	if err != nil || code != 0 {
		srcPath = tempName + ":" + extractPath
		_, errStr, code, err = d.runCollect(ctx, "cp", srcPath, destDir+"/")
		if err != nil || code != 0 {
			return fmt.Errorf("failed to copy files from container: %s", strings.TrimSpace(errStr))
		}
	}

	if d.debug {
		fmt.Printf("[docker] extracted files from %s:%s to %s\n", imageRef, extractPath, destDir)
	}

	return nil
}

func (d Docker) updateRestartNo(ctx context.Context, name string) {
	_, _, _, _ = d.runCollect(ctx, "update", "--restart=no", name)
}

func (d Docker) updateRestartPolicy(ctx context.Context, name, policy string) {
	clean := strings.ToLower(strings.TrimSpace(policy))
	switch clean {
	case "", "no", "always", "unless-stopped", "on-failure":
	default:
		clean = "unless-stopped"
	}
	if clean == "" {
		clean = "no"
	}
	_, _, _, _ = d.runCollect(ctx, "update", "--restart="+clean, name)
}

func (d Docker) getImageDigest(ctx context.Context, imageRef string) string {
	out, _, code, err := d.runCollect(ctx, "inspect", "--format", "{{.Id}}", imageRef)
	if err != nil || code != 0 {
		return ""
	}
	return strings.TrimSpace(out)
}

func (a *Agent) syncNewFilesToServer(ctx context.Context, imageRef, serverDir string, containerPaths []string) error {
	tmpDir, err := os.MkdirTemp("", "adpanel-sync-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	extracted := false
	for _, containerPath := range containerPaths {
		if err := a.docker.extractImageFiles(ctx, imageRef, tmpDir, containerPath); err == nil {
			extracted = true
			break
		}
	}

	if !extracted {
		commonPaths := []string{"/app", "/data", "/config", "/opt", "/srv"}
		for _, p := range commonPaths {
			if err := a.docker.extractImageFiles(ctx, imageRef, tmpDir, p); err == nil {
				extracted = true
				break
			}
		}
	}

	if !extracted {
		return fmt.Errorf("could not extract files from image")
	}

	newFiles := 0
	err = filepath.Walk(tmpDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() {
			return walkErr
		}

		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		rel, err := filepath.Rel(tmpDir, path)
		if err != nil {
			return nil
		}

		destPath := filepath.Join(serverDir, rel)

		destParent := filepath.Dir(destPath)
		if rp, rpErr := filepath.EvalSymlinks(destParent); rpErr == nil && !isUnder(serverDir, rp) {
			return nil
		}
		if di, diErr := os.Lstat(destPath); diErr == nil && di.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if _, err := os.Stat(destPath); os.IsNotExist(err) {
			if err := ensureDir(filepath.Dir(destPath)); err != nil {
				return nil
			}

			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}

			if err := writeFileNoFollow(destPath, data, info.Mode()); err == nil {
				newFiles++
				if a.debug {
					fmt.Printf("[sync] added new file: %s\n", rel)
				}
			}
		}

		return nil
	})

	if newFiles > 0 {
		fmt.Printf("[sync] added %d new files from image update\n", newFiles)
	}

	return err
}

func serverDirVisibleFileCount(serverDir string) int {
	entries, err := os.ReadDir(serverDir)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		name := entry.Name()
		if name == ".adpanel" || name == ".adpanel_meta" || name == "adpanel.json" || strings.HasPrefix(name, ".adpanel_") {
			continue
		}
		count++
	}
	return count
}

func appendUniqueContainerPath(paths []string, seen map[string]struct{}, raw string) []string {
	clean := path.Clean(strings.TrimSpace(raw))
	if clean == "" || clean == "." || clean == "/" || !strings.HasPrefix(clean, "/") {
		return paths
	}
	if _, ok := seen[clean]; ok {
		return paths
	}
	seen[clean] = struct{}{}
	return append(paths, clean)
}

func commandReferencedContainerPaths(command, workdir, dataDir string) []string {
	args := parseShellArgs(command)
	if len(args) == 0 {
		return nil
	}
	paths := []string{}
	seen := map[string]struct{}{}
	for _, arg := range args[1:] {
		token := strings.TrimSpace(arg)
		if token == "" || strings.HasPrefix(token, "-") {
			continue
		}
		lower := strings.ToLower(token)
		if strings.HasPrefix(token, "/") {
			paths = appendUniqueContainerPath(paths, seen, path.Dir(token))
			continue
		}
		if strings.Contains(token, "/") || strings.HasSuffix(lower, ".sh") || strings.HasSuffix(lower, ".py") || strings.HasSuffix(lower, ".js") {
			base := strings.TrimSpace(workdir)
			if base == "" {
				base = dataDir
			}
			if base != "" {
				paths = appendUniqueContainerPath(paths, seen, path.Dir(path.Join(base, token)))
			}
		}
	}
	return paths
}

func (a *Agent) prepareServerDirFromImage(ctx context.Context, imageRef, serverDir string, imgCfg imageConfig, paths []string) string {
	if serverDirVisibleFileCount(serverDir) > 0 {
		return ""
	}

	seen := map[string]struct{}{}
	candidates := []string{}
	for _, p := range paths {
		candidates = appendUniqueContainerPath(candidates, seen, p)
	}
	for _, p := range imgCfg.Volumes {
		candidates = appendUniqueContainerPath(candidates, seen, p)
	}
	for _, p := range getDataPathsFromEnv(imgCfg.Env) {
		candidates = appendUniqueContainerPath(candidates, seen, p)
	}
	candidates = appendUniqueContainerPath(candidates, seen, imgCfg.Workdir)
	for _, p := range []string{"/app", "/data", "/config", "/srv", "/opt"} {
		candidates = appendUniqueContainerPath(candidates, seen, p)
	}

	for _, p := range candidates {
		extractCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		err := a.docker.extractImageFiles(extractCtx, imageRef, serverDir, p)
		cancel()
		if err != nil {
			continue
		}
		if serverDirVisibleFileCount(serverDir) > 0 {
			_ = sanitizeExtractedTree(serverDir)
			ensureVolumeWritable(serverDir)
			fmt.Printf("[docker] prepared server directory from image %s:%s\n", imageRef, p)
			return p
		}
	}
	return ""
}

func (d Docker) stop(ctx context.Context, name string) {
	_, _, _, _ = d.runCollect(ctx, "stop", "-t", "15", name)
}

const (
	stdinMaxContainers = 100
	stdinIdleTimeout   = 10 * time.Minute
	stdinWriteTimeout  = 5 * time.Second
	stdinCommandMaxLen = 4096
	stdinBufferSize    = 256

	consoleCommandDuplicateWindow  = 1200 * time.Millisecond
	consoleCommandRequestIDWindow  = 30 * time.Second
	consoleCommandRequestIDMaxLen  = 128
	consoleCommandRecentMaxServers = 1000
)

type recentConsoleCommand struct {
	command   string
	requestID string
	at        time.Time
}

type stdinConn struct {
	name      string
	stdin     io.WriteCloser
	cmd       *exec.Cmd
	lastUsed  time.Time
	tty       bool
	transport string
	mu        sync.Mutex
	closed    bool
}

type stdinManager struct {
	mu       sync.RWMutex
	conns    map[string]*stdinConn
	docker   *Docker
	debug    bool
	stopCh   chan struct{}
	stopOnce sync.Once
}

func preferDockerCLIConsoleTransport() bool {
	override := strings.ToLower(strings.TrimSpace(os.Getenv("ADPANEL_DOCKER_STREAM_TRANSPORT")))
	switch override {
	case "cli", "docker-cli", "docker":
		return true
	case "api", "docker-api", "hijack":
		return false
	}
	return false
}

func isEnterpriseLinuxOSRelease(raw string) bool {
	osInfo := strings.ToLower(raw)
	for _, line := range strings.Split(osInfo, "\n") {
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if key != "id" && key != "id_like" {
			continue
		}
		for _, distro := range []string{"rocky", "rhel", "centos", "almalinux", "ol", "fedora"} {
			if val == distro || strings.Contains(val, distro) {
				return true
			}
		}
	}
	return false
}

func preferDockerCLIPTYStdinTransport() bool {
	override := strings.ToLower(strings.TrimSpace(os.Getenv("ADPANEL_DOCKER_STDIN_TRANSPORT")))
	switch override {
	case "pty", "cli-pty", "docker-pty", "script":
		return true
	case "api", "docker-api", "hijack":
		return false
	}
	return true
}

func containerTTYForHost() bool {
	return true
}

func dockerRunInteractiveArgs() []string {
	return []string{"run", "-d", "-i", "-t"}
}

func dockerRunInteractivePrefix() string {
	return "docker run -d -i -t"
}

func newStdinManager(docker *Docker, debug bool) *stdinManager {
	sm := &stdinManager{
		conns:  make(map[string]*stdinConn),
		docker: docker,
		debug:  debug,
		stopCh: make(chan struct{}),
	}
	go sm.cleanupLoop()
	return sm
}

func (sm *stdinManager) cleanupLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-sm.stopCh:
			return
		case <-ticker.C:
			sm.cleanupIdle()
		}
	}
}

func extractTarSafeRouted(tr *tar.Reader, serverDir string, backupsDir string, metaDir ...string) error {
	var totalSize int64
	var fileCount int

	const backupsPrefix = ".adpanel_backups/"
	const metaPrefix = ".adpanel_meta/"

	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		fileCount++
		if fileCount > maxExtractFiles {
			return fmt.Errorf("too many files in tar: %d (max %d)", fileCount, maxExtractFiles)
		}
		if h.Size > maxSingleFile {
			return fmt.Errorf("file too large in tar: %s (%d bytes, max %d)", h.Name, h.Size, maxSingleFile)
		}

		cleaned := filepath.Clean(h.Name)
		cleaned = strings.TrimPrefix(cleaned, "/")
		if cleaned == "." || cleaned == "" {
			continue
		}

		destRoot := serverDir
		rel := cleaned
		if cleaned == strings.TrimSuffix(backupsPrefix, "/") {
			continue
		}
		if cleaned == strings.TrimSuffix(metaPrefix, "/") {
			continue
		}
		if strings.HasPrefix(cleaned, metaPrefix) {
			if len(metaDir) > 0 && metaDir[0] != "" {
				destRoot = metaDir[0]
				rel = strings.TrimPrefix(cleaned, metaPrefix)
				if rel == "" || rel == "." {
					continue
				}
			} else {
				continue
			}
		} else if strings.HasPrefix(cleaned, backupsPrefix) {
			destRoot = backupsDir
			rel = strings.TrimPrefix(cleaned, backupsPrefix)
			if rel == "" || rel == "." {
				continue
			}
		}

		target := filepath.Join(destRoot, rel)
		if !isUnder(destRoot, target) {
			return fmt.Errorf("tar-slip blocked: %s", h.Name)
		}
		baseName := filepath.Base(rel)
		if baseName == "." || baseName == ".." {
			continue
		}

		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			resolvedDir, rdErr := filepath.EvalSymlinks(target)
			if rdErr == nil && !isUnder(destRoot, resolvedDir) {
				return fmt.Errorf("symlink escape blocked during tar extract: %s", h.Name)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			resolvedParent, rpErr := filepath.EvalSymlinks(filepath.Dir(target))
			if rpErr == nil && !isUnder(destRoot, resolvedParent) {
				return fmt.Errorf("symlink escape blocked during tar extract: %s", h.Name)
			}
			if ei, eiErr := os.Lstat(target); eiErr == nil && ei.Mode()&os.ModeSymlink != 0 {
				continue
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW, 0o644)
			if err != nil {
				return err
			}
			lw := &limitedWriter{w: out, limit: maxExtractSize - totalSize}
			n, err := io.Copy(lw, tr)
			out.Close()
			if err != nil {
				return err
			}
			totalSize += n
		case tar.TypeLink:
			return fmt.Errorf("hardlinks not allowed for security: %s", h.Name)
		case tar.TypeSymlink:
			continue
		default:
			continue
		}
	}
}

func (sm *stdinManager) cleanupIdle() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := time.Now()
	for name, conn := range sm.conns {
		conn.mu.Lock()
		idle := now.Sub(conn.lastUsed) > stdinIdleTimeout
		conn.mu.Unlock()

		if idle {
			if sm.debug {
				fmt.Printf("[stdin-manager] closing idle connection for %s\n", name)
			}
			conn.close()
			delete(sm.conns, name)
		}
	}
}

func (sm *stdinManager) stop() {
	sm.stopOnce.Do(func() {
		close(sm.stopCh)
		sm.mu.Lock()
		defer sm.mu.Unlock()
		for name, conn := range sm.conns {
			conn.close()
			delete(sm.conns, name)
		}
	})
}

func (sm *stdinManager) getOrCreate(ctx context.Context, containerName string) (*stdinConn, error) {
	name := dockerContainerName(containerName)
	if name == "" {
		return nil, fmt.Errorf("empty container name")
	}

	if !sm.docker.containerExists(ctx, name) {
		return nil, fmt.Errorf("container-not-running")
	}

	sm.mu.Lock()
	defer sm.mu.Unlock()

	if conn, ok := sm.conns[name]; ok {
		conn.mu.Lock()
		if !conn.closed {
			conn.lastUsed = time.Now()
			conn.mu.Unlock()
			return conn, nil
		}
		conn.mu.Unlock()
		delete(sm.conns, name)
	}

	if len(sm.conns) >= stdinMaxContainers {
		var oldestName string
		var oldestTime time.Time
		for n, c := range sm.conns {
			c.mu.Lock()
			if oldestName == "" || c.lastUsed.Before(oldestTime) {
				oldestName = n
				oldestTime = c.lastUsed
			}
			c.mu.Unlock()
		}
		if oldestName != "" {
			if sm.debug {
				fmt.Printf("[stdin-manager] evicting oldest connection %s\n", oldestName)
			}
			sm.conns[oldestName].close()
			delete(sm.conns, oldestName)
		}
	}

	conn, err := sm.createStdinConn(ctx, name)
	if err != nil {
		return nil, err
	}
	sm.conns[name] = conn
	return conn, nil
}

func (sm *stdinManager) createStdinConn(ctx context.Context, name string) (*stdinConn, error) {
	tty := sm.docker.containerTTY(ctx, name)
	preferCLI := preferDockerCLIConsoleTransport() && !tty
	preferCLIPTY := tty && preferDockerCLIPTYStdinTransport()
	if preferCLIPTY {
		conn, err := sm.createDockerCLIPTYStdinConn(ctx, name)
		if err == nil {
			return conn, nil
		}
		if sm.debug {
			fmt.Printf("[stdin-manager] Docker CLI PTY stdin attach failed for %s: %v\n", name, err)
		}
	}
	if preferCLI {
		conn, err := sm.createDockerCLIStdinConn(ctx, name)
		if err == nil {
			return conn, nil
		}
		if sm.debug {
			fmt.Printf("[stdin-manager] Docker CLI stdin attach failed for %s: %v\n", name, err)
		}
	}

	attachCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	stdin, err := sm.docker.attachContainerAPI(attachCtx, name, dockerAttachOptions{Stdin: true})
	if err != nil {
		if tty && !preferCLIPTY {
			conn, ptyErr := sm.createDockerCLIPTYStdinConn(ctx, name)
			if ptyErr == nil {
				return conn, nil
			}
			return nil, fmt.Errorf("failed to attach container stdin: Docker API: %v; Docker CLI PTY: %w", err, ptyErr)
		}
		if !preferCLI && !tty {
			conn, cliErr := sm.createDockerCLIStdinConn(ctx, name)
			if cliErr == nil {
				return conn, nil
			}
			return nil, fmt.Errorf("failed to attach container stdin: Docker API: %v; Docker CLI: %w", err, cliErr)
		}
		return nil, fmt.Errorf("failed to attach container stdin: %w", err)
	}

	conn := &stdinConn{
		name:      name,
		stdin:     stdin,
		lastUsed:  time.Now(),
		tty:       tty,
		transport: "docker-api",
	}

	if sm.debug {
		fmt.Printf("[stdin-manager] created Docker API stdin connection for %s (tty=%t)\n", name, conn.tty)
	}

	return conn, nil
}

func (sm *stdinManager) createDockerCLIPTYStdinConn(ctx context.Context, name string) (*stdinConn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("empty container name")
	}

	cmd := exec.Command("docker", "attach", "--sig-proxy=false", "--detach-keys=ctrl-@", name)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 30, Cols: 120})
	if err != nil {
		return nil, err
	}

	conn := &stdinConn{
		name:      name,
		stdin:     ptmx,
		cmd:       cmd,
		lastUsed:  time.Now(),
		tty:       true,
		transport: "docker-cli-pty",
	}
	go func() {
		_, _ = io.Copy(io.Discard, ptmx)
	}()
	go func() {
		_ = cmd.Wait()
		conn.mu.Lock()
		conn.closed = true
		conn.mu.Unlock()
	}()

	if sm.debug {
		fmt.Printf("[stdin-manager] created Docker CLI PTY stdin connection for %s\n", name)
	}
	return conn, nil
}

func (sm *stdinManager) createDockerCLIStdinConn(ctx context.Context, name string) (*stdinConn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("empty container name")
	}

	cmd := exec.Command("docker", "attach", "--sig-proxy=false", name)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, err
	}

	conn := &stdinConn{
		name:      name,
		stdin:     stdin,
		cmd:       cmd,
		lastUsed:  time.Now(),
		tty:       sm.docker.containerTTY(ctx, name),
		transport: "docker-cli",
	}
	go func() {
		_ = cmd.Wait()
		conn.mu.Lock()
		conn.closed = true
		conn.mu.Unlock()
	}()

	if sm.debug {
		fmt.Printf("[stdin-manager] created Docker CLI stdin connection for %s (tty=%t)\n", name, conn.tty)
	}
	return conn, nil
}

func (c *stdinConn) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	if c.stdin != nil {
		c.stdin.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
}

func (c *stdinConn) sendCommand(cmd string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("connection closed")
	}
	c.lastUsed = time.Now()
	line := strings.TrimRight(cmd, "\r\n") + "\n"
	if deadlineWriter, ok := c.stdin.(interface{ SetWriteDeadline(time.Time) error }); ok {
		_ = deadlineWriter.SetWriteDeadline(time.Now().Add(stdinWriteTimeout))
		defer deadlineWriter.SetWriteDeadline(time.Time{})
	}
	if _, err := io.WriteString(c.stdin, line); err != nil {
		c.closed = true
		if c.stdin != nil {
			_ = c.stdin.Close()
		}
		if c.cmd != nil && c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		return fmt.Errorf("stdin write failed: %w", err)
	}
	return nil
}

func (sm *stdinManager) remove(name string) {
	name = dockerContainerName(name)
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if conn, ok := sm.conns[name]; ok {
		conn.close()
		delete(sm.conns, name)
	}
}

func exitCodeFromErr(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func (d Docker) writeToContainerProcessStdin(ctx context.Context, containerName, command string) error {
	name := dockerContainerName(containerName)
	cmd := strings.TrimSpace(command)
	if name == "" || cmd == "" {
		return fmt.Errorf("missing-params")
	}
	shells := []string{"sh", "/bin/sh", "bash", "/bin/bash"}
	for idx, shellPath := range shells {
		script := "fd=$(readlink /proc/1/fd/0 2>/dev/null || true); case \"$fd\" in /dev/null*) exit 45 ;; esac; cat > /proc/1/fd/0"
		pipeCmd := exec.CommandContext(ctx, "docker", "exec", "-i", name, shellPath, "-lc", script)
		pipeCmd.Stdin = strings.NewReader(cmd + "\n")
		pipeCmd.Stdout = io.Discard
		var stderr bytes.Buffer
		pipeCmd.Stderr = &stderr
		if err := pipeCmd.Run(); err == nil {
			return nil
		} else {
			exitCode := exitCodeFromErr(err)
			if exitCode == 45 {
				return fmt.Errorf("process stdin is not open; restart the server so the container is recreated with interactive stdin")
			}
			if isMissingExecShell(stderr.String(), shellPath, exitCode) && idx < len(shells)-1 {
				continue
			}
			detail := strings.TrimSpace(stderr.String())
			if detail == "" {
				detail = err.Error()
			}
			return fmt.Errorf("process stdin write failed: %s", detail)
		}
	}
	return fmt.Errorf("process stdin write failed: no supported shell found in container")
}

func (d Docker) containerTTY(ctx context.Context, containerName string) bool {
	name := dockerContainerName(containerName)
	if name == "" {
		return false
	}
	inspectCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, _, _, err := d.runCollect(inspectCtx, "inspect", "-f", "{{.Config.Tty}}", name)
	return err == nil && strings.EqualFold(strings.TrimSpace(out), "true")
}

func (d Docker) containerOpenStdin(ctx context.Context, containerName string) bool {
	name := dockerContainerName(containerName)
	if name == "" {
		return false
	}
	inspectCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, _, _, err := d.runCollect(inspectCtx, "inspect", "-f", "{{.Config.OpenStdin}} {{.Config.AttachStdin}}", name)
	if err != nil {
		return false
	}
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(out)))
	return len(fields) >= 1 && fields[0] == "true"
}

func (d Docker) writeToMinecraftProcessStdin(ctx context.Context, containerName, command string) error {
	name := dockerContainerName(containerName)
	cmd := strings.TrimSpace(command)
	if name == "" || cmd == "" {
		return fmt.Errorf("missing-params")
	}
	script := "pid=$(pgrep -n java || pgrep -n bedrock_server || echo 1); fd=$(readlink \"/proc/$pid/fd/0\" 2>/dev/null || true); case \"$fd\" in /dev/null*) exit 45 ;; esac; cat > \"/proc/$pid/fd/0\""
	shells := []string{"sh", "/bin/sh", "bash", "/bin/bash"}
	for idx, shellPath := range shells {
		pipeCmd := exec.CommandContext(ctx, "docker", "exec", "-i", name, shellPath, "-lc", script)
		pipeCmd.Stdin = strings.NewReader(cmd + "\n")
		pipeCmd.Stdout = io.Discard
		var stderr bytes.Buffer
		pipeCmd.Stderr = &stderr
		if err := pipeCmd.Run(); err == nil {
			return nil
		} else {
			exitCode := exitCodeFromErr(err)
			if exitCode == 45 {
				return fmt.Errorf("minecraft stdin is not open; restart the server so the container is recreated with interactive stdin")
			}
			if isMissingExecShell(stderr.String(), shellPath, exitCode) && idx < len(shells)-1 {
				continue
			}
			detail := strings.TrimSpace(stderr.String())
			if detail == "" {
				detail = err.Error()
			}
			return fmt.Errorf("minecraft stdin write failed: %s", detail)
		}
	}
	return fmt.Errorf("minecraft stdin write failed: no supported shell found in container")
}

func (d Docker) writeToTargetProcessStdin(ctx context.Context, containerName, command string, targets []string) error {
	name := dockerContainerName(containerName)
	cmd := strings.TrimSpace(command)
	targets = sanitizeConsoleProcessTargets(targets)
	if name == "" || cmd == "" {
		return fmt.Errorf("missing-params")
	}
	if len(targets) == 0 {
		return fmt.Errorf("no console target processes configured")
	}

	script := strings.Join([]string{
		"pid=''",
		"self=$$",
		"for pattern in \"$@\"; do",
		"  [ -n \"$pattern\" ] || continue",
		"  pid=$(pgrep -n -x -- \"$pattern\" 2>/dev/null || true)",
		"  if [ -z \"$pid\" ]; then",
		"    for candidate in $(pgrep -f -- \"$pattern\" 2>/dev/null || true); do",
		"      [ \"$candidate\" = \"$self\" ] && continue",
		"      cmdline=$(tr '\\000' ' ' < \"/proc/$candidate/cmdline\" 2>/dev/null || true)",
		"      case \"$cmdline\" in *adpanel-console*) continue ;; esac",
		"      pid=$candidate",
		"    done",
		"  fi",
		"  [ -n \"$pid\" ] && break",
		"done",
		"[ -n \"$pid\" ] || exit 44",
		"fd=$(readlink \"/proc/$pid/fd/0\" 2>/dev/null || true)",
		"case \"$fd\" in /dev/null*) exit 45 ;; esac",
		"cat > \"/proc/$pid/fd/0\"",
	}, "\n")

	shells := []string{"sh", "/bin/sh", "bash", "/bin/bash"}
	for idx, shellPath := range shells {
		args := []string{"exec", "-i", name, shellPath, "-c", script, "adpanel-console"}
		args = append(args, targets...)
		pipeCmd := exec.CommandContext(ctx, "docker", args...)
		pipeCmd.Stdin = strings.NewReader(cmd + "\n")
		pipeCmd.Stdout = io.Discard
		var stderr bytes.Buffer
		pipeCmd.Stderr = &stderr
		if err := pipeCmd.Run(); err == nil {
			if d.debug {
				fmt.Printf("[docker] command sent to target process stdin: %s\n", strings.Join(targets, ","))
			}
			return nil
		} else {
			exitCode := exitCodeFromErr(err)
			if exitCode == 44 {
				return fmt.Errorf("target process not found: %s", strings.Join(targets, ", "))
			}
			if exitCode == 45 {
				return fmt.Errorf("target process stdin is not open; restart the server so the container is recreated with interactive stdin")
			}
			if isMissingExecShell(stderr.String(), shellPath, exitCode) && idx < len(shells)-1 {
				continue
			}
			detail := strings.TrimSpace(stderr.String())
			if detail == "" {
				detail = err.Error()
			}
			return fmt.Errorf("target process stdin write failed: %s", detail)
		}
	}
	return fmt.Errorf("target process stdin write failed: no supported shell found in container")
}

func (d Docker) sendViaAttachedStdin(ctx context.Context, stdinMgr *stdinManager, containerName, command string) error {
	name := dockerContainerName(containerName)
	cmd := strings.TrimSpace(command)
	if name == "" || cmd == "" {
		return fmt.Errorf("missing-params")
	}
	if stdinMgr == nil {
		return fmt.Errorf("stdin manager unavailable")
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		conn, err := stdinMgr.getOrCreate(ctx, name)
		if err == nil {
			if err := conn.sendCommand(cmd); err == nil {
				if d.debug {
					fmt.Printf("[docker] command sent via attached container stdin\n")
				}
				return nil
			} else {
				lastErr = err
				stdinMgr.remove(name)
				continue
			}
		}
		lastErr = err
		stdinMgr.remove(name)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("stdin channel unavailable")
	}
	return lastErr
}

func (d Docker) sendCommand(ctx context.Context, stdinMgr *stdinManager, containerName, command string, targetProcesses []string) error {
	name := dockerContainerName(containerName)
	cmd := strings.TrimSpace(command)
	if name == "" || cmd == "" {
		return fmt.Errorf("missing-params")
	}

	if len(cmd) > stdinCommandMaxLen {
		return fmt.Errorf("command too long (max %d)", stdinCommandMaxLen)
	}

	if err := validateCommandInput(cmd); err != nil {
		return err
	}

	if state := d.containerState(ctx, name); state != "running" {
		return fmt.Errorf("container-not-running")
	}

	openStdin := d.containerOpenStdin(ctx, name)
	if !openStdin {
		if stdinMgr != nil {
			stdinMgr.remove(name)
		}
		return fmt.Errorf("container stdin is not open; restart the server so the container is recreated with interactive stdin")
	}

	tty := d.containerTTY(ctx, name)

	var targetErr error
	var attachedErr error
	if stdinMgr != nil {
		if err := d.sendViaAttachedStdin(ctx, stdinMgr, name, cmd); err == nil {
			return nil
		} else {
			attachedErr = err
			if d.debug {
				fmt.Printf("[docker] attached stdin failed for %s: %v\n", name, err)
			}
		}
	}

	if tty {
		if attachedErr != nil {
			return fmt.Errorf("failed-to-send: attached TTY stdin failed: %w", attachedErr)
		}
		return fmt.Errorf("failed-to-send: TTY stdin channel unavailable")
	}

	if len(targetProcesses) > 0 {
		if err := d.writeToTargetProcessStdin(ctx, name, cmd, targetProcesses); err == nil {
			return nil
		} else {
			targetErr = err
			if d.debug {
				fmt.Printf("[docker] target process stdin failed for %s: %v\n", name, err)
			}
		}
	}

	if err := d.writeToContainerProcessStdin(ctx, name, cmd); err == nil {
		if d.debug {
			fmt.Printf("[docker] command sent via /proc/1/fd/0\n")
		}
		return nil
	} else if err != nil {
		if attachedErr != nil {
			if targetErr != nil {
				return fmt.Errorf("failed-to-send: target process stdin failed: %v; attached stdin failed: %v; process stdin fallback failed: %w", targetErr, attachedErr, err)
			}
			return fmt.Errorf("failed-to-send: attached stdin failed: %v; process stdin fallback failed: %w", attachedErr, err)
		}
		if targetErr != nil {
			return fmt.Errorf("failed-to-send: target process stdin failed: %v; process stdin fallback failed: %w", targetErr, err)
		}
		return fmt.Errorf("failed-to-send: %w", err)
	}
	return fmt.Errorf("failed-to-send: stdin channel unavailable")
}

func validateCommandInput(cmd string) error {
	if strings.ContainsRune(cmd, '\x00') {
		return fmt.Errorf("invalid command: contains null byte")
	}
	for _, r := range cmd {
		if r < 32 && r != '\t' && r != '\n' && r != '\r' {
			return fmt.Errorf("invalid command: contains control character")
		}
	}
	return nil
}

type Downloader struct {
	headerTimeout time.Duration
	maxBytes      int64
}

func (dl Downloader) safeURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("invalid scheme")
	}
	if u.User != nil && u.User.String() != "" {
		return nil, fmt.Errorf("credentials not allowed")
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("missing host")
	}
	if isBlockedOutboundHostname(host) {
		return nil, fmt.Errorf("blocked host")
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("dns failed: %w", err)
	}
	checkedIP := false
	for _, ip := range ips {
		addr, ok := netip.AddrFromSlice(ip)
		if !ok {
			continue
		}
		checkedIP = true
		if isBlockedOutboundAddr(addr) {
			return nil, fmt.Errorf("blocked host ip: %s", addr.String())
		}
	}
	if !checkedIP {
		return nil, fmt.Errorf("dns returned no usable ip")
	}
	return u, nil
}

func isBlockedOutboundHostname(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	return h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local")
}

func isBlockedOutboundAddr(addr netip.Addr) bool {
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	return !addr.IsGlobalUnicast() ||
		addr.IsPrivate() ||
		addr.IsLoopback() ||
		addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() ||
		addr.IsMulticast() ||
		addr.IsUnspecified()
}

func isPrivateIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	return isBlockedOutboundAddr(addr)
}

func (dl Downloader) client() *http.Client {
	return dl.clientWithProxy(http.ProxyFromEnvironment)
}

func (dl Downloader) clientNoProxy() *http.Client {
	return dl.clientWithProxy(nil)
}

func (dl Downloader) clientWithProxy(proxy func(*http.Request) (*url.URL, error)) *http.Client {
	safeDialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	transport := &http.Transport{
		Proxy: proxy,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("invalid address: %w", err)
			}

			ips, err := net.LookupIP(host)
			if err != nil {
				return nil, fmt.Errorf("dns lookup failed: %w", err)
			}

			for _, ip := range ips {
				if isPrivateIP(ip) {
					continue
				}
				dialAddr := net.JoinHostPort(ip.String(), port)
				return safeDialer.DialContext(ctx, network, dialAddr)
			}

			return nil, fmt.Errorf("all resolved IPs are blocked (private/internal)")
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: dl.headerTimeout,
		IdleConnTimeout:       60 * time.Second,
		MaxIdleConns:          50,
		MaxIdleConnsPerHost:   10,
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			_, err := dl.safeURL(req.URL.String())
			return err
		},
	}
}

func (dl Downloader) getJSON(ctx context.Context, raw string, dst any) error {
	u, err := dl.safeURL(raw)
	if err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	resp, err := dl.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, dst)
}

func (dl Downloader) downloadToFile(ctx context.Context, rawURL, destPath string) error {
	u, err := dl.safeURL(rawURL)
	if err != nil {
		return err
	}
	if err := ensureDir(filepath.Dir(destPath)); err != nil {
		return err
	}

	req, _ := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	resp, err := dl.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("http %d", resp.StatusCode)
	}

	tmp := destPath + ".tmp"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	var r io.Reader = resp.Body
	if dl.maxBytes > 0 {
		r = io.LimitReader(resp.Body, dl.maxBytes)
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, destPath)
}

func writeMinecraftScaffold(serverDir, name, fork, version string) {
	_ = os.WriteFile(filepath.Join(serverDir, "eula.txt"), []byte("eula=true\n"), 0o644)
	props := strings.Join([]string{
		"motd=" + name,
		"max-players=20",
		"enforce-secure-profile=false",
		"server-port=25565",
		"server-ip=",
		"",
	}, "\n")
	_ = os.WriteFile(filepath.Join(serverDir, "server.properties"), []byte(props), 0o644)
}

func enforceServerProps(serverDir string) {
	p := filepath.Join(serverDir, "server.properties")
	txt := ""
	if b, err := os.ReadFile(p); err == nil {
		txt = string(b)
	}
	if txt == "" {
		txt = "server-port=25565\nserver-ip=\n"
	} else {
		rePort := regexp.MustCompile(`(?m)^server-port=.*$`)
		reIP := regexp.MustCompile(`(?m)^server-ip=.*$`)
		if rePort.MatchString(txt) {
			txt = rePort.ReplaceAllString(txt, "server-port=25565")
		} else {
			txt += "\nserver-port=25565\n"
		}
		if reIP.MatchString(txt) {
			txt = reIP.ReplaceAllString(txt, "server-ip=")
		} else {
			txt += "server-ip=\n"
		}
	}
	_ = os.WriteFile(p, []byte(txt), 0o644)
}

func fixedJarUrlFor1218(fork string) string {
	f := strings.ToLower(strings.TrimSpace(fork))
	switch f {
	case "paper":
		return "https://fill-data.papermc.io/v1/objects/8de7c52c3b02403503d16fac58003f1efef7dd7a0256786843927fa92ee57f1e/paper-1.21.8-60.jar"
	case "pufferfish":
		return "https://ci.pufferfish.host/job/Pufferfish-1.21/33/artifact/pufferfish-server/build/libs/pufferfish-paperclip-1.21.8-R0.1-SNAPSHOT-mojmap.jar"
	case "vanilla":
		return "https://piston-data.mojang.com/v1/objects/95495a7f485eedd84ce928cef5e223b757d2f764/server.jar"
	default:
		return "https://api.purpurmc.org/v2/purpur/1.21.8/2497/download"
	}
}

func getMinecraftJarURL(ctx context.Context, dl Downloader, fork, version string, debug bool) string {
	v := strings.TrimSpace(version)
	f := strings.ToLower(strings.TrimSpace(fork))
	if v == "1.21.8" {
		return fixedJarUrlFor1218(f)
	}

	defer func() {
		_ = recover()
	}()

	if f == "purpur" {
		return fmt.Sprintf("https://api.purpurmc.org/v2/purpur/%s/latest/download", v)
	}

	if f == "paper" || f == "pufferfish" {
		var builds struct {
			Builds []struct {
				Build     int `json:"build"`
				Downloads struct {
					Application struct {
						Name string `json:"name"`
					} `json:"application"`
				} `json:"downloads"`
			} `json:"builds"`
		}
		u := fmt.Sprintf("https://api.papermc.io/v2/projects/paper/versions/%s/builds", v)
		if err := dl.getJSON(ctx, u, &builds); err == nil && len(builds.Builds) > 0 {
			last := builds.Builds[len(builds.Builds)-1]
			jarName := last.Downloads.Application.Name
			if jarName == "" {
				jarName = fmt.Sprintf("paper-%s-%d.jar", v, last.Build)
			}
			return fmt.Sprintf("https://api.papermc.io/v2/projects/paper/versions/%s/builds/%d/downloads/%s", v, last.Build, jarName)
		}
	}

	if f == "vanilla" {
		var manifest struct {
			Versions []struct {
				ID  string `json:"id"`
				URL string `json:"url"`
			} `json:"versions"`
		}
		if err := dl.getJSON(ctx, "https://piston-meta.mojang.com/mc/game/version_manifest_v2.json", &manifest); err == nil {
			for _, ver := range manifest.Versions {
				if ver.ID == v && ver.URL != "" {
					var det struct {
						Downloads struct {
							Server struct {
								URL string `json:"url"`
							} `json:"server"`
						} `json:"downloads"`
					}
					if err := dl.getJSON(ctx, ver.URL, &det); err == nil && det.Downloads.Server.URL != "" {
						return det.Downloads.Server.URL
					}
				}
			}
		}
	}

	return fmt.Sprintf("https://api.purpurmc.org/v2/purpur/%s/latest/download", v)
}

const legacyPythonMainTemplate = `def greet(name="World"):
    return f"Hello, {name}!"

if __name__ == "__main__":
    print("--- Starting main.py execution ---")

    user_name = "ADPanel"
    message_1 = greet(user_name)
    print(f"Message 1: {message_1}")

    message_2 = greet()
    print(f"Message 2: {message_2}")

    print("--- Execution finished ---")
`

const pythonMainTemplate = `from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import os


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        body = b"ADPanel Python server is running.\n"
        self.send_response(200)
        self.send_header("Content-Type", "text/plain; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt, *args):
        print("%s - %s" % (self.address_string(), fmt % args), flush=True)


if __name__ == "__main__":
    port = int(os.environ.get("PORT", "8000"))
    server = ThreadingHTTPServer(("0.0.0.0", port), Handler)
    print(f"ADPanel Python server listening on 0.0.0.0:{port}", flush=True)
    server.serve_forever()
`

func clampPort(p any, fallback int) int {
	if n, ok := parseStrictPortValue(p); ok {
		return n
	}
	return fallback
}

func isStrictNumericPortString(s string) bool {
	if s == "" {
		return false
	}
	if s[0] < '1' || s[0] > '9' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func parseStrictPortValue(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		if x < 1 || x > 65535 {
			return 0, false
		}
		return x, true
	case int64:
		if x < 1 || x > 65535 {
			return 0, false
		}
		return int(x), true
	case float64:
		if x < 1 || x > 65535 || x != float64(int(x)) {
			return 0, false
		}
		return int(x), true
	case float32:
		if x < 1 || x > 65535 || x != float32(int(x)) {
			return 0, false
		}
		return int(x), true
	case string:
		s := strings.TrimSpace(x)
		if !isStrictNumericPortString(s) {
			return 0, false
		}
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 65535 {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

func normalizePortProtocolName(v any) string {
	protocol := strings.ToLower(strings.TrimSpace(fmt.Sprint(v)))
	switch protocol {
	case "tcp", "udp", "sctp":
		return protocol
	default:
		return ""
	}
}

func normalizePortProtocolSlice(v any) []string {
	var raw []any
	switch x := v.(type) {
	case []string:
		raw = make([]any, 0, len(x))
		for _, item := range x {
			raw = append(raw, item)
		}
	case []any:
		raw = x
	case string:
		raw = []any{x}
	default:
		return nil
	}

	out := make([]string, 0, len(raw))
	seen := map[string]struct{}{}
	for _, item := range raw {
		protocol := normalizePortProtocolName(item)
		if protocol == "" {
			continue
		}
		if _, ok := seen[protocol]; ok {
			continue
		}
		seen[protocol] = struct{}{}
		out = append(out, protocol)
	}
	return out
}

func runtimePortProtocolMap(raw any) map[int][]string {
	out := map[int][]string{}
	switch x := raw.(type) {
	case map[string][]string:
		for key, protocolsRaw := range x {
			port, ok := parseStrictPortValue(key)
			if !ok {
				continue
			}
			protocols := normalizePortProtocolSlice(protocolsRaw)
			if len(protocols) > 0 {
				out[port] = protocols
			}
		}
	case map[string]any:
		for key, protocolsRaw := range x {
			port, ok := parseStrictPortValue(key)
			if !ok {
				continue
			}
			protocols := normalizePortProtocolSlice(protocolsRaw)
			if len(protocols) > 0 {
				out[port] = protocols
			}
		}
	case map[int][]string:
		for port, protocolsRaw := range x {
			if _, ok := parseStrictPortValue(port); !ok {
				continue
			}
			protocols := normalizePortProtocolSlice(protocolsRaw)
			if len(protocols) > 0 {
				out[port] = protocols
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func portProtocolsFor(protocolsByPort map[int][]string, port int) []string {
	if port > 0 {
		if protocols := protocolsByPort[port]; len(protocols) > 0 {
			return protocols
		}
	}
	return []string{"tcp"}
}

func mergePortProtocols(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	seen := map[string]struct{}{}
	for _, source := range [][]string{a, b} {
		for _, raw := range source {
			protocol := normalizePortProtocolName(raw)
			if protocol == "" {
				continue
			}
			if _, ok := seen[protocol]; ok {
				continue
			}
			seen[protocol] = struct{}{}
			out = append(out, protocol)
		}
	}
	return out
}

func sanitizeDockerPortProtocols(raw map[string][]string, ports []int) map[string][]string {
	if len(raw) == 0 {
		return nil
	}
	allowedPorts := map[int]struct{}{}
	for _, port := range ports {
		if _, ok := parseStrictPortValue(port); ok {
			allowedPorts[port] = struct{}{}
		}
	}

	out := map[string][]string{}
	for rawPort, protocolsRaw := range raw {
		port, ok := parseStrictPortValue(rawPort)
		if !ok {
			continue
		}
		if len(allowedPorts) > 0 {
			if _, ok := allowedPorts[port]; !ok {
				continue
			}
		}
		protocols := normalizePortProtocolSlice(protocolsRaw)
		if len(protocols) > 0 {
			out[strconv.Itoa(port)] = protocols
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (a *Agent) dockerHostPortOwner(ctx context.Context, port int, excludeName string) string {
	if a == nil || port <= 0 {
		return ""
	}
	out, _, code, err := a.docker.runCollect(ctx, "ps", "--format", "{{.Names}}\t{{.Ports}}")
	if err != nil || code != 0 {
		return ""
	}
	needle := ":" + strconv.Itoa(port) + "->"
	excluded := strings.ToLower(strings.TrimSpace(excludeName))
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		if excluded != "" && strings.ToLower(name) == excluded {
			continue
		}
		if strings.Contains(parts[1], needle) {
			return name
		}
	}
	return ""
}

func tcpHostPortBindable(port int) bool {
	if port <= 0 {
		return true
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

func udpHostPortBindable(port int) bool {
	if port <= 0 {
		return true
	}
	ln, err := net.ListenPacket("udp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

func hostPortBindableForProtocols(port int, protocols []string) bool {
	if port <= 0 {
		return true
	}
	cleanProtocols := normalizePortProtocolSlice(protocols)
	if len(cleanProtocols) == 0 {
		cleanProtocols = []string{"tcp"}
	}
	for _, protocol := range cleanProtocols {
		switch protocol {
		case "udp":
			if !udpHostPortBindable(port) {
				return false
			}
		case "tcp", "sctp":
			if !tcpHostPortBindable(port) {
				return false
			}
		}
	}
	return true
}

func (a *Agent) hostPortAvailability(ctx context.Context, port int, excludeName string) (bool, string, string) {
	return a.hostPortAvailabilityForProtocols(ctx, port, []string{"tcp"}, excludeName)
}

func (a *Agent) hostPortAvailabilityForProtocols(ctx context.Context, port int, protocols []string, excludeName string) (bool, string, string) {
	if port <= 0 {
		return true, "", ""
	}
	if owner := a.dockerHostPortOwner(ctx, port, excludeName); owner != "" {
		return false, owner, "docker"
	}
	if !hostPortBindableForProtocols(port, protocols) {
		return false, "", "socket"
	}
	return true, "", ""
}

var dockerRunFlagsWithValue = map[string]bool{
	"-e": true, "--env": true, "-v": true, "--volume": true,
	"-p": true, "--publish": true, "-w": true, "--workdir": true,
	"--name": true, "-m": true, "--memory": true, "--cpus": true,
	"--memory-swap": true, "--memory-reservation": true, "--cpu-period": true, "--cpu-quota": true,
	"-u": true, "--user": true, "-h": true, "--hostname": true,
	"--network": true, "--net": true, "--restart": true,
	"-l": true, "--label": true, "--entrypoint": true,
	"--mount": true, "--tmpfs": true, "--device": true,
	"--pid": true, "--ipc": true, "--userns": true, "--uts": true, "--cgroupns": true,
	"--shm-size": true, "--ulimit": true, "--dns": true, "--dns-search": true,
	"--add-host": true, "--log-driver": true, "--log-opt": true,
	"--stop-signal": true, "--stop-timeout": true, "--health-cmd": true,
	"--health-interval": true, "--health-retries": true, "--health-timeout": true,
	"--runtime": true, "--platform": true, "--pull": true,
	"--ip": true, "--ip6": true, "--mac-address": true,
	"--expose": true, "--link": true, "--cidfile": true,
}

var dockerRunSafeStandaloneFlags = map[string]bool{
	"-d": true, "--detach": true, "-i": true, "--interactive": true,
	"-t": true, "--tty": true, "--rm": true, "--init": true, "--read-only": true,
}

var dockerRunBlockedFlags = map[string]bool{
	"--privileged":         true,
	"--cap-add":            true,
	"--cap-drop":           true,
	"--security-opt":       true,
	"--volumes-from":       true,
	"--env-file":           true,
	"--device":             true,
	"--device-cgroup-rule": true,
	"--sysctl":             true,
	"--oom-kill-disable":   true,
	"--oom-score-adj":      true,
}

var dockerRunHostNamespaceFlags = map[string]bool{
	"--network": true, "--net": true, "--pid": true, "--userns": true,
	"--ipc": true, "--uts": true, "--cgroupns": true,
}

func isSafeShortDockerFlagCluster(flag string) bool {
	if len(flag) < 2 || flag[0] != '-' || strings.HasPrefix(flag, "--") {
		return false
	}
	for i := 1; i < len(flag); i++ {
		switch flag[i] {
		case 'd', 'D', 'i', 'I', 't', 'T':
		default:
			return false
		}
	}
	return true
}

func parseDockerFlagToken(token string) (string, string) {
	trimmed := strings.TrimSpace(token)
	lower := strings.ToLower(trimmed)
	if eqIdx := strings.Index(lower, "="); eqIdx > 0 {
		return lower[:eqIdx], strings.TrimSpace(trimmed[eqIdx+1:])
	}
	return lower, ""
}

func readDockerFlagValue(args []string, index int, inlineValue string) (string, int) {
	if strings.TrimSpace(inlineValue) != "" {
		return inlineValue, index
	}
	if index+1 >= len(args) {
		return "", index
	}
	return args[index+1], index + 1
}

func validateDockerRunCommandSafety(command string, allowedHostPorts []int, reservedHostPorts []int, expectedImageRef string) string {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return ""
	}
	if !strings.HasPrefix(strings.ToLower(cmd), "docker run") {
		return "Startup command must start with 'docker run'."
	}
	for _, r := range cmd {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
			return "Startup command contains unsupported control characters."
		}
	}

	allowedSet := make(map[int]bool, len(allowedHostPorts))
	for _, p := range allowedHostPorts {
		if port, ok := parseStrictPortValue(p); ok {
			allowedSet[port] = true
		}
	}
	reservedSet := make(map[int]bool, len(reservedHostPorts))
	for _, p := range reservedHostPorts {
		if port, ok := parseStrictPortValue(p); ok {
			reservedSet[port] = true
		}
	}

	args := parseShellArgs(strings.TrimSpace(cmd[len("docker run"):]))
	for i := 0; i < len(args); i++ {
		token := strings.TrimSpace(args[i])
		if token == "" {
			continue
		}
		tokenLower := strings.ToLower(token)
		if !strings.HasPrefix(tokenLower, "-") {
			if strings.TrimSpace(expectedImageRef) != "" {
				normalizedExpected := normalizeDockerImage(expectedImageRef)
				normalizedCommand := normalizeDockerImage(token)
				if normalizedExpected != "" && normalizedCommand != "" && normalizedExpected != normalizedCommand {
					return fmt.Sprintf("The Docker image in the startup command (%s) does not match the configured image (%s).", token, expectedImageRef)
				}
			}
			return ""
		}

		flagName, inlineValue := parseDockerFlagToken(token)

		if dockerRunBlockedFlags[flagName] {
			return fmt.Sprintf("Blocked dangerous Docker flag: %s", flagName)
		}

		if flagName == "--mount" {
			value, nextIndex := readDockerFlagValue(args, i, inlineValue)
			mountValue := strings.ToLower(strings.TrimSpace(value))
			if mountValue == "" {
				return "Docker flag --mount requires a value."
			}
			if strings.Contains(mountValue, "type=bind") {
				return "Blocked dangerous Docker flag: --mount with bind"
			}
			i = nextIndex
			continue
		}

		if flagName == "--tmpfs" {
			value, nextIndex := readDockerFlagValue(args, i, inlineValue)
			tmpfsValue := strings.ToLower(strings.TrimSpace(value))
			if tmpfsValue == "" {
				return "Docker flag --tmpfs requires a value."
			}
			if strings.Contains(tmpfsValue, "exec") {
				return "Blocked dangerous Docker flag: --tmpfs with exec"
			}
			i = nextIndex
			continue
		}

		if dockerRunHostNamespaceFlags[flagName] {
			value, nextIndex := readDockerFlagValue(args, i, inlineValue)
			nsMode := strings.ToLower(strings.TrimSpace(value))
			if nsMode == "" {
				return fmt.Sprintf("Docker flag %s requires a value.", flagName)
			}
			if nsMode == "host" {
				return fmt.Sprintf("Blocked dangerous Docker flag: %s=host", flagName)
			}
			i = nextIndex
			continue
		}

		if flagName == "-p" || flagName == "--publish" {
			value, nextIndex := readDockerFlagValue(args, i, inlineValue)
			mapping := strings.TrimSpace(value)
			if mapping == "" {
				return "Docker publish flag is missing a port mapping."
			}
			if !strings.Contains(mapping, "{PORT}") {
				hostPort := extractHostPortFromMapping(mapping)
				if hostPort == 0 {
					return fmt.Sprintf("Invalid Docker port mapping: %s", mapping)
				}
				if !allowedSet[hostPort] {
					return "Port config through docker edit command is not allowed. Add or remove a port through Port Management."
				}
				if reservedSet[hostPort] {
					return fmt.Sprintf("Port %d in the Docker command conflicts with a port forwarding rule.", hostPort)
				}
			}
			i = nextIndex
			continue
		}

		if isSafeShortDockerFlagCluster(flagName) || dockerRunSafeStandaloneFlags[flagName] {
			continue
		}

		if dockerRunFlagsWithValue[flagName] {
			value, nextIndex := readDockerFlagValue(args, i, inlineValue)
			if strings.TrimSpace(value) == "" {
				return fmt.Sprintf("Docker flag %s requires a value.", flagName)
			}
			i = nextIndex
			continue
		}

		return fmt.Sprintf("Unsupported Docker flag in startup command: %s", flagName)
	}

	return "Startup command must include a Docker image."
}

func buildNodeIndexTemplate(port int) string {
	p := port
	if p < 1 || p > 65535 {
		p = 3001
	}
	return fmt.Sprintf(`const http = require("http");

const PORT = Number(process.env.PORT || %d);

const server = http.createServer((req, res) => {
  const body = "ADPanel Node.js server is running.\n";
  res.writeHead(200, {
    "Content-Type": "text/plain; charset=utf-8",
    "Content-Length": Buffer.byteLength(body)
  });
  res.end(body);
});

server.listen(PORT, "0.0.0.0", () => {
  console.log(`+"`"+`ADPanel Node.js server listening on 0.0.0.0:${PORT}`+"`"+`);
});
`, p)
}

func writeGeneratedRuntimeFile(path string, content []byte, isReplaceableLegacy func([]byte) bool) {
	if strings.TrimSpace(path) == "" {
		return
	}
	if existing, err := os.ReadFile(path); err == nil {
		if isReplaceableLegacy != nil && isReplaceableLegacy(existing) {
			_ = os.WriteFile(path, content, 0o644)
		}
		return
	}
	_ = ensureDir(filepath.Dir(path))
	_ = os.WriteFile(path, content, 0o644)
}

func isLegacyPythonScaffold(content []byte) bool {
	return strings.TrimSpace(string(content)) == strings.TrimSpace(legacyPythonMainTemplate)
}

func isLegacyNodeScaffold(content []byte) bool {
	s := string(content)
	return strings.Contains(s, `const express = require("express");`) &&
		strings.Contains(s, "Hello World from ADPanel!")
}

func scaffoldPython(serverDir, startFile string) {
	sf := strings.TrimSpace(startFile)
	if sf == "" {
		sf = "main.py"
	}
	sf = filepath.Base(sf)
	if sf == "." || sf == ".." || sf == "" {
		sf = "main.py"
	}
	writeGeneratedRuntimeFile(filepath.Join(serverDir, sf), []byte(pythonMainTemplate), isLegacyPythonScaffold)
}

func scaffoldNode(serverDir, startFile string, port int) {
	sf := strings.TrimSpace(startFile)
	if sf == "" {
		sf = "index.js"
	}
	sf = filepath.Base(sf)
	if sf == "." || sf == ".." || sf == "" {
		sf = "index.js"
	}
	writeGeneratedRuntimeFile(filepath.Join(serverDir, sf), []byte(buildNodeIndexTemplate(port)), isLegacyNodeScaffold)

	pkgPath := filepath.Join(serverDir, "package.json")
	if _, err := os.Stat(pkgPath); err == nil {
		return
	}

	pkg := map[string]any{
		"name":         filepath.Base(serverDir),
		"private":      true,
		"version":      "1.0.0",
		"main":         "index.js",
		"scripts":      map[string]string{"start": "node index.js"},
		"dependencies": map[string]string{},
	}
	b, _ := json.MarshalIndent(pkg, "", "  ")
	_ = os.WriteFile(pkgPath, b, 0o644)
}

var ansiSGRRE = regexp.MustCompile(`\x1b\[[0-9;:]*m`)
var mcHexRE = regexp.MustCompile(`§x(?:§[0-9A-Fa-f]){6}`)
var mcRE = regexp.MustCompile(`§[0-9A-FK-ORa-fk-or]`)
var promptArtifactRE = regexp.MustCompile(`(?m)^\s*>\.{2,}\s*`)
var ttyOnlyLineRE = regexp.MustCompile(`^[\s>.=…-]+$`)

func stripDockerStreamFraming(s string) string {
	buf := []byte(s)
	if len(buf) < 8 {
		return s
	}
	out := make([]byte, 0, len(buf))
	usedFrames := false
	for i := 0; i < len(buf); {
		if len(buf)-i >= 8 && buf[i] >= 1 && buf[i] <= 3 && buf[i+1] == 0 && buf[i+2] == 0 && buf[i+3] == 0 {
			frameLen := int(binary.BigEndian.Uint32(buf[i+4 : i+8]))
			if frameLen == 0 {
				usedFrames = true
				i += 8
				continue
			}
			if frameLen > 0 && len(buf)-i-8 >= frameLen {
				out = append(out, buf[i+8:i+8+frameLen]...)
				i += 8 + frameLen
				usedFrames = true
				continue
			}
		}
		out = append(out, buf[i])
		i++
	}
	if usedFrames {
		return string(out)
	}
	return s
}

func consumeAnsiCSISequence(s string, idx int) (next int, keep bool) {
	i := idx
	for i < len(s) {
		c := s[i]
		if c >= 0x40 && c <= 0x7e {
			return i + 1, c == 'm'
		}
		i++
	}
	return len(s), false
}

func skipAnsiOSCSequence(s string, idx int) int {
	i := idx
	for i < len(s) {
		if s[i] == 0x07 {
			return i + 1
		}
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
			return i + 2
		}
		i++
	}
	return len(s)
}

func skipAnsiEscTerminatedSequence(s string, idx int) int {
	i := idx
	for i < len(s) {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
			return i + 2
		}
		i++
	}
	return len(s)
}

func sanitizeAnsiForConsole(s string) string {
	if s == "" {
		return s
	}
	var out strings.Builder
	out.Grow(len(s))
	for i := 0; i < len(s); {
		switch s[i] {
		case 0x9b:
			next, keep := consumeAnsiCSISequence(s, i+1)
			if keep {
				out.WriteString(s[i:next])
			}
			i = next
			continue
		case 0x1b:
			if i+1 >= len(s) {
				i++
				continue
			}
			nextByte := s[i+1]
			switch nextByte {
			case '[':
				next, keep := consumeAnsiCSISequence(s, i+2)
				if keep {
					out.WriteString(s[i:next])
				}
				i = next
				continue
			case ']':
				i = skipAnsiOSCSequence(s, i+2)
				continue
			case 'P', 'X', '^', '_':
				i = skipAnsiEscTerminatedSequence(s, i+2)
				continue
			default:
				if nextByte >= 0x40 && nextByte <= 0x5f {
					i += 2
					continue
				}
				i++
				continue
			}
		default:
			if (s[i] < 32 || s[i] == 127) && s[i] != '\n' && s[i] != '\r' && s[i] != '\t' {
				i++
				continue
			}
			out.WriteByte(s[i])
			i++
		}
	}
	return out.String()
}

func cleanLogLine(s string) string {
	if s == "" {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = stripDockerStreamFraming(s)
	s = sanitizeAnsiForConsole(s)
	s = mcHexRE.ReplaceAllString(s, "")
	s = mcRE.ReplaceAllString(s, "")
	s = promptArtifactRE.ReplaceAllString(s, "")
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.ReplaceAll(line, "…", "")
		line = strings.TrimRight(line, " \t")
		line = strings.TrimLeft(line, ">=. ")
		plain := strings.TrimSpace(ansiSGRRE.ReplaceAllString(line, ""))
		if plain == "" || ttyOnlyLineRE.MatchString(plain) {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func normalizeConsoleMode(raw any) string {
	switch v := raw.(type) {
	case string:
		mode := strings.ToLower(strings.TrimSpace(v))
		switch mode {
		case "stdin", "exec", "exec-shell", "shell":
			return mode
		}
	case map[string]any:
		if mode := normalizeConsoleMode(v["mode"]); mode != "" {
			return mode
		}
		if mode := normalizeConsoleMode(v["type"]); mode != "" {
			return mode
		}
	}
	return ""
}

func consoleProcessTargetsFromRaw(raw any) []string {
	switch v := raw.(type) {
	case string:
		return sanitizeConsoleProcessTargets([]string{v})
	case []string:
		return sanitizeConsoleProcessTargets(v)
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			out = append(out, fmt.Sprint(item))
		}
		return sanitizeConsoleProcessTargets(out)
	case map[string]any:
		for _, key := range []string{"targetProcesses", "target_processes", "processes", "processNames", "process_names"} {
			if targets := consoleProcessTargetsFromRaw(v[key]); len(targets) > 0 {
				return targets
			}
		}
		for _, key := range []string{"targetProcess", "target_process", "process", "target"} {
			if targets := consoleProcessTargetsFromRaw(v[key]); len(targets) > 0 {
				return targets
			}
		}
	}
	return nil
}

func sanitizeConsoleProcessTargets(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		target := strings.TrimSpace(value)
		target = strings.Trim(target, "'\"")
		if target == "" || target == "<nil>" || strings.ContainsAny(target, "\x00\r\n") {
			continue
		}
		if len(target) > 120 {
			target = target[:120]
		}
		if _, ok := seen[target]; ok {
			continue
		}
		seen[target] = struct{}{}
		out = append(out, target)
		if len(out) >= 12 {
			break
		}
	}
	return out
}

func normalizeTemplateID(raw any) string {
	id := strings.ToLower(strings.TrimSpace(fmt.Sprint(raw)))
	id = strings.ReplaceAll(id, "_", "-")
	id = strings.ReplaceAll(id, " ", "-")
	for strings.Contains(id, "--") {
		id = strings.ReplaceAll(id, "--", "-")
	}
	id = strings.Trim(id, "-")
	if id == "<nil>" {
		return ""
	}
	return id
}

func isKnownStdinConsoleTemplate(id string) bool {
	switch normalizeTemplateID(id) {
	case "7-days-to-die", "7dtd", "sdtd",
		"ark", "ark-survival-evolved",
		"assetto-corsa",
		"csgo", "cs2", "counter-strike-2",
		"dayz",
		"fivem", "five-m",
		"hytale",
		"minecraft",
		"palworld",
		"project-zomboid", "zomboid",
		"ragemp", "rage-mp",
		"redm",
		"rust",
		"valheim":
		return true
	default:
		return false
	}
}

func defaultConsoleProcessTargetsForTemplate(id string) []string {
	switch normalizeTemplateID(id) {
	case "7-days-to-die", "7dtd", "sdtd":
		return []string{"7DaysToDieServer", "7DaysToDieServer.x86_64"}
	case "ark", "ark-survival-evolved":
		return []string{"ShooterGameServer"}
	case "assetto-corsa":
		return []string{"server-manager", "acServer"}
	case "csgo", "cs2", "counter-strike-2":
		return []string{"cs2", "srcds_linux"}
	case "dayz":
		return []string{"DayZServer", "DayZServer_x64", "dayzserver"}
	case "fivem", "five-m":
		return []string{"FXServer", "fxserver"}
	case "hytale":
		return []string{"hytale-server", "java"}
	case "minecraft":
		return []string{"java", "bedrock_server"}
	case "palworld":
		return []string{"PalServer-Linux-Test", "PalServer"}
	case "project-zomboid", "zomboid":
		return []string{"ProjectZomboid64", "ProjectZomboid32", "java"}
	case "ragemp", "rage-mp":
		return []string{"ragemp-server", "ragemp"}
	case "redm":
		return []string{"FXServer", "fxserver"}
	case "rust":
		return []string{"RustDedicated"}
	case "valheim":
		return []string{"valheim_server", "valheim_server.x86_64"}
	default:
		return nil
	}
}

func defaultConsoleProcessTargetsForImage(image string) []string {
	imageLower := strings.ToLower(strings.TrimSpace(image))
	switch {
	case strings.Contains(imageLower, "hytale"):
		return defaultConsoleProcessTargetsForTemplate("hytale")
	case strings.Contains(imageLower, "redm"):
		return defaultConsoleProcessTargetsForTemplate("redm")
	case strings.Contains(imageLower, "fivem"):
		return defaultConsoleProcessTargetsForTemplate("fivem")
	case strings.Contains(imageLower, "dayz") || strings.Contains(imageLower, "day-z"):
		return defaultConsoleProcessTargetsForTemplate("dayz")
	case strings.Contains(imageLower, "zomboid"):
		return defaultConsoleProcessTargetsForTemplate("project-zomboid")
	case strings.Contains(imageLower, "palworld"):
		return defaultConsoleProcessTargetsForTemplate("palworld")
	case strings.Contains(imageLower, "valheim"):
		return defaultConsoleProcessTargetsForTemplate("valheim")
	case strings.Contains(imageLower, "rust"):
		return defaultConsoleProcessTargetsForTemplate("rust")
	case strings.Contains(imageLower, "ragemp"):
		return defaultConsoleProcessTargetsForTemplate("ragemp")
	case strings.Contains(imageLower, "ark"):
		return defaultConsoleProcessTargetsForTemplate("ark-survival-evolved")
	case strings.Contains(imageLower, "7dtd") || strings.Contains(imageLower, "7daystodie") || strings.Contains(imageLower, "7-days-to-die") || strings.Contains(imageLower, "sdtd"):
		return defaultConsoleProcessTargetsForTemplate("7-days-to-die")
	case strings.Contains(imageLower, "assetto"):
		return defaultConsoleProcessTargetsForTemplate("assetto-corsa")
	case strings.Contains(imageLower, "cs2") || strings.Contains(imageLower, "csgo"):
		return defaultConsoleProcessTargetsForTemplate("cs2")
	default:
		return nil
	}
}

func consoleProcessTargetsForServer(meta map[string]any) []string {
	runtimeObj, _ := meta["runtime"].(map[string]any)
	if runtimeObj != nil {
		if targets := consoleProcessTargetsFromRaw(runtimeObj["console"]); len(targets) > 0 {
			return targets
		}
	}
	for _, key := range []string{"template", "type"} {
		if targets := defaultConsoleProcessTargetsForTemplate(fmt.Sprint(meta[key])); len(targets) > 0 {
			return targets
		}
	}
	if runtimeObj != nil {
		if targets := defaultConsoleProcessTargetsForTemplate(fmt.Sprint(runtimeObj["providerId"])); len(targets) > 0 {
			return targets
		}
		if targets := defaultConsoleProcessTargetsForImage(fmt.Sprint(runtimeObj["image"])); len(targets) > 0 {
			return targets
		}
	}
	return nil
}

func inferredConsoleModeForServer(meta map[string]any) string {
	runtimeObj, _ := meta["runtime"].(map[string]any)
	if runtimeObj != nil {
		if mode := normalizeConsoleMode(runtimeObj["console"]); mode != "" {
			return mode
		}
		if len(consoleProcessTargetsFromRaw(runtimeObj["console"])) > 0 {
			return "stdin"
		}
	}

	for _, key := range []string{"template", "type"} {
		if isKnownStdinConsoleTemplate(fmt.Sprint(meta[key])) {
			return "stdin"
		}
	}
	if runtimeObj != nil {
		if isKnownStdinConsoleTemplate(fmt.Sprint(runtimeObj["providerId"])) {
			return "stdin"
		}
		image := strings.TrimSpace(fmt.Sprint(runtimeObj["image"]))
		if image != "" && image != "<nil>" && isGameServerDockerImage(image) {
			return "stdin"
		}
		startupDisplay := strings.TrimSpace(fmt.Sprint(runtimeObj["startupDisplay"]))
		if startupDisplay != "" && startupDisplay != "<nil>" {
			return "stdin"
		}
	}
	return ""
}

const (
	maxExtractSize  = 10 << 30
	maxExtractFiles = 100000
	maxSingleFile   = 2 << 30
)

type limitedWriter struct {
	w       io.Writer
	written int64
	limit   int64
}

func (lw *limitedWriter) Write(p []byte) (int, error) {
	if lw.written+int64(len(p)) > lw.limit {
		return 0, fmt.Errorf("extraction size limit exceeded (%d bytes)", lw.limit)
	}
	n, err := lw.w.Write(p)
	lw.written += int64(n)
	return n, err
}

func sanitizeFileMode(mode os.FileMode, isDir bool) os.FileMode {
	mode &^= os.ModeSetuid | os.ModeSetgid | os.ModeSticky
	if isDir {
		mode = mode.Perm() & 0o755
	} else {
		mode = mode.Perm() & 0o644
	}
	return mode
}

func sanitizeExtractedTree(dest string) error {
	return filepath.Walk(dest, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return nil
		}
		safe := sanitizeFileMode(info.Mode(), info.IsDir())
		if info.Mode().Perm() != safe {
			_ = os.Chmod(path, safe)
		}
		return nil
	})
}

func extractZipSafe(src, dest string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()

	if len(r.File) > maxExtractFiles {
		return fmt.Errorf("too many files in archive: %d (max %d)", len(r.File), maxExtractFiles)
	}

	var totalSize int64
	var fileCount int

	for _, f := range r.File {
		name := f.Name
		name = filepath.FromSlash(name)
		target := filepath.Join(dest, name)
		if !isUnder(dest, target) {
			return fmt.Errorf("zip-slip blocked: %s", f.Name)
		}

		if f.UncompressedSize64 > maxSingleFile {
			return fmt.Errorf("file too large: %s (%d bytes, max %d)", f.Name, f.UncompressedSize64, maxSingleFile)
		}

		baseName := filepath.Base(name)
		if baseName == "." || baseName == ".." {
			continue
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			resolvedDir, rdErr := filepath.EvalSymlinks(target)
			if rdErr == nil && !isUnder(dest, resolvedDir) {
				return fmt.Errorf("symlink escape blocked during zip extract: %s", f.Name)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		resolvedParent, rpErr := filepath.EvalSymlinks(filepath.Dir(target))
		if rpErr == nil && !isUnder(dest, resolvedParent) {
			return fmt.Errorf("symlink escape blocked during zip extract: %s", f.Name)
		}
		if ei, eiErr := os.Lstat(target); eiErr == nil && ei.Mode()&os.ModeSymlink != 0 {
			continue
		}

		if err := func() error {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			defer rc.Close()

			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW, 0o644)
			if err != nil {
				return err
			}
			defer out.Close()

			lw := &limitedWriter{w: out, limit: maxExtractSize - totalSize}
			n, err := io.Copy(lw, rc)
			if err != nil {
				return err
			}
			totalSize += n
			fileCount++

			if fileCount > maxExtractFiles {
				return fmt.Errorf("too many files extracted: %d (max %d)", fileCount, maxExtractFiles)
			}

			return nil
		}(); err != nil {
			return err
		}
	}
	return nil
}

func extractTarSafe(tr *tar.Reader, dest string) error {
	var totalSize int64
	var fileCount int

	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		fileCount++
		if fileCount > maxExtractFiles {
			return fmt.Errorf("too many files in tar: %d (max %d)", fileCount, maxExtractFiles)
		}

		if h.Size > maxSingleFile {
			return fmt.Errorf("file too large in tar: %s (%d bytes, max %d)", h.Name, h.Size, maxSingleFile)
		}

		name := filepath.Clean(h.Name)
		baseName := filepath.Base(name)
		if baseName == "." || baseName == ".." {
			continue
		}
		target := filepath.Join(dest, name)
		if !isUnder(dest, target) {
			return fmt.Errorf("tar-slip blocked: %s", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			resolvedDir, rdErr := filepath.EvalSymlinks(target)
			if rdErr == nil && !isUnder(dest, resolvedDir) {
				return fmt.Errorf("symlink escape blocked during tar extract: %s", h.Name)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			resolvedParent, rpErr := filepath.EvalSymlinks(filepath.Dir(target))
			if rpErr == nil && !isUnder(dest, resolvedParent) {
				return fmt.Errorf("symlink escape blocked during tar extract: %s", h.Name)
			}
			if ei, eiErr := os.Lstat(target); eiErr == nil && ei.Mode()&os.ModeSymlink != 0 {
				continue
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW, 0o644)
			if err != nil {
				return err
			}

			lw := &limitedWriter{w: out, limit: maxExtractSize - totalSize}
			n, err := io.Copy(lw, tr)
			out.Close()
			if err != nil {
				return err
			}
			totalSize += n

		case tar.TypeLink:
			return fmt.Errorf("hardlinks not allowed for security: %s", h.Name)

		case tar.TypeSymlink:
			continue

		default:
			continue
		}
	}
}

func detectArchiveType(p string) string {
	l := strings.ToLower(p)
	switch {
	case strings.HasSuffix(l, ".zip"):
		return "zip"
	case strings.HasSuffix(l, ".tar"):
		return "tar"
	case strings.HasSuffix(l, ".tar.gz") || strings.HasSuffix(l, ".tgz"):
		return "targz"
	case strings.HasSuffix(l, ".tar.bz2") || strings.HasSuffix(l, ".tbz2"):
		return "tarbz2"
	case strings.HasSuffix(l, ".tar.xz") || strings.HasSuffix(l, ".txz"):
		return "tarxz"
	case strings.HasSuffix(l, ".7z"):
		return "7z"
	case strings.HasSuffix(l, ".rar"):
		return "rar"
	default:
		return ""
	}
}

type AuditLogger struct {
	enabled bool
	mu      sync.Mutex
}

func sanitizeLogField(s string) string {
	return strings.NewReplacer(
		"\n", "\\n",
		"\r", "\\r",
		"\t", "\\t",
	).Replace(s)
}

func (al *AuditLogger) Log(event, clientIP, user, resource, action, result string, details map[string]any) {
	if !al.enabled {
		return
	}
	al.mu.Lock()
	defer al.mu.Unlock()

	entry := map[string]any{
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"event":     sanitizeLogField(event),
		"clientIP":  sanitizeLogField(clientIP),
		"user":      sanitizeLogField(user),
		"resource":  sanitizeLogField(resource),
		"action":    sanitizeLogField(action),
		"result":    sanitizeLogField(result),
	}
	if details != nil {
		entry["details"] = details
	}
	b, _ := json.Marshal(entry)
	fmt.Printf("[AUDIT] %s\n", string(b))
}

var auditLog = &AuditLogger{enabled: true}

type Agent struct {
	cfg         Config
	debug       bool
	token       string
	tokenID     string
	uuid        string
	apiHost     string
	apiPort     int
	uploadLimit int64
	dataRoot    string
	volumesDir  string
	sftpPort    int

	panelHMAC string

	stopGrace time.Duration

	docker Docker
	dl     Downloader

	logs     *logManager
	stdinMgr *stdinManager

	resourceStats *serverResourceStatsCache

	stopMu      sync.Mutex
	stoppingNow map[string]time.Time

	commandMu      sync.Mutex
	recentCommands map[string]recentConsoleCommand

	importProgressMu sync.RWMutex
	importProgress   map[string]*ImportProgress

	importSem chan struct{}

	wsMu          sync.Mutex
	wsSubscribers map[*wsSubscriber]struct{}

	metaLocks sync.Map

	serverOpLocks sync.Map
}

const (
	serverResourceStatsDefaultTTL = 2500 * time.Millisecond
	serverResourceStatsDiskTTL    = 15 * time.Second
	serverResourceStatsMaxIdle    = 10 * time.Minute
)

type serverResourceStatsCache struct {
	ttl     time.Duration
	diskTTL time.Duration
	mu      sync.Mutex
	entries map[string]*serverResourceStatsCacheEntry
}

type serverResourceStatsCacheEntry struct {
	mu sync.Mutex

	payload   map[string]any
	expiresAt time.Time
	touchedAt time.Time

	diskBytes     int64
	diskExpiresAt time.Time

	containerID string
	seenNetwork bool
	lastRawRx   uint64
	lastRawTx   uint64
	totalRx     uint64
	totalTx     uint64

	networkWindowStartedAt time.Time
	networkWindowRx        uint64
	networkWindowTx        uint64
}

func newServerResourceStatsCache(ttl, diskTTL time.Duration) *serverResourceStatsCache {
	if ttl <= 0 {
		ttl = serverResourceStatsDefaultTTL
	}
	if diskTTL <= 0 {
		diskTTL = serverResourceStatsDiskTTL
	}
	return &serverResourceStatsCache{
		ttl:     ttl,
		diskTTL: diskTTL,
		entries: make(map[string]*serverResourceStatsCacheEntry),
	}
}

func (c *serverResourceStatsCache) get(name string) *serverResourceStatsCacheEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[name]
	if entry == nil {
		entry = &serverResourceStatsCacheEntry{}
		c.entries[name] = entry
	}
	return entry
}

func (c *serverResourceStatsCache) cleanup(maxIdle time.Duration) {
	if c == nil || maxIdle <= 0 {
		return
	}
	cutoff := time.Now().Add(-maxIdle)
	c.mu.Lock()
	defer c.mu.Unlock()
	for name, entry := range c.entries {
		entry.mu.Lock()
		idle := !entry.touchedAt.IsZero() && entry.touchedAt.Before(cutoff)
		entry.mu.Unlock()
		if idle {
			delete(c.entries, name)
		}
	}
}

func (e *serverResourceStatsCacheEntry) cachedDiskUsage(a *Agent, serverDir string, now time.Time, ttl time.Duration) int64 {
	if now.Before(e.diskExpiresAt) {
		return e.diskBytes
	}
	e.diskBytes = a.getServerDiskUsage(serverDir)
	e.diskExpiresAt = now.Add(ttl)
	return e.diskBytes
}

func addUint64Saturating(a, b uint64) uint64 {
	if ^uint64(0)-a < b {
		return ^uint64(0)
	}
	return a + b
}

func (e *serverResourceStatsCacheEntry) updateNetworkCounters(containerID string, rawRx, rawTx uint64, now time.Time, resetEvery time.Duration) (uint64, uint64, uint64, uint64, time.Time) {
	if e.seenNetwork && containerID != "" && e.containerID != "" && containerID != e.containerID {
		e.lastRawRx = 0
		e.lastRawTx = 0
	}

	var rxDelta, txDelta uint64
	if !e.seenNetwork {
		e.totalRx = rawRx
		e.totalTx = rawTx
		rxDelta = rawRx
		txDelta = rawTx
	} else {
		if rawRx >= e.lastRawRx {
			rxDelta = rawRx - e.lastRawRx
		} else {
			rxDelta = rawRx
		}
		if rawTx >= e.lastRawTx {
			txDelta = rawTx - e.lastRawTx
		} else {
			txDelta = rawTx
		}
		e.totalRx = addUint64Saturating(e.totalRx, rxDelta)
		e.totalTx = addUint64Saturating(e.totalTx, txDelta)
	}

	if resetEvery > 0 {
		if e.networkWindowStartedAt.IsZero() || now.Sub(e.networkWindowStartedAt) >= resetEvery || now.Before(e.networkWindowStartedAt) {
			e.networkWindowStartedAt = now
			e.networkWindowRx = 0
			e.networkWindowTx = 0
		}
		e.networkWindowRx = addUint64Saturating(e.networkWindowRx, rxDelta)
		e.networkWindowTx = addUint64Saturating(e.networkWindowTx, txDelta)
	}

	e.lastRawRx = rawRx
	e.lastRawTx = rawTx
	e.containerID = containerID
	e.seenNetwork = true
	return e.totalRx, e.totalTx, e.networkWindowRx, e.networkWindowTx, e.networkWindowStartedAt
}

type ImportProgress struct {
	Status     string `json:"status"`
	Percent    int    `json:"percent"`
	Downloaded int64  `json:"downloaded"`
	Total      int64  `json:"total"`
	Error      string `json:"error,omitempty"`
}

func (a *Agent) logf(format string, args ...any) {
	if a.debug {
		fmt.Printf(format+"\n", args...)
	}
}

func (a *Agent) authOK(r *http.Request) bool {
	h := r.Header.Get("x-node-token")
	if h == "" {
		h = r.Header.Get("authorization")
	}
	t := strings.TrimSpace(h)
	if strings.HasPrefix(strings.ToLower(t), "bearer ") {
		t = strings.TrimSpace(t[7:])
	}
	if a.token == "" || t == "" {
		return false
	}
	return timingSafeEq(t, a.token)
}

func (a *Agent) verifyPanelSignature(r *http.Request, name string, providerId, versionId, u string, bodyTS any) (bool, string) {
	if a.panelHMAC == "" {
		return false, "hmac-not-configured"
	}
	ts := r.Header.Get("x-panel-ts")
	sig := r.Header.Get("x-panel-sign")
	if ts == "" || sig == "" {
		return false, "missing-signature"
	}
	tsNum, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false, "expired"
	}
	now := time.Now().UnixMilli()
	drift := now - tsNum
	if drift < 0 {
		drift = -drift
	}
	if drift > 5*60*1000 {
		return false, "expired"
	}
	base := fmt.Sprintf("%s|%s|%s|%s|%s", strings.TrimSpace(name), providerId, versionId, u, ts)
	m := hmac.New(sha256.New, []byte(a.panelHMAC))
	_, _ = m.Write([]byte(base))
	expect := hex.EncodeToString(m.Sum(nil))
	if !timingSafeEq(strings.TrimSpace(sig), expect) {
		return false, "bad-signature"
	}
	_ = bodyTS
	return true, ""
}

func (a *Agent) verifyReinstallAdminRequest(r *http.Request, name, template string) (bool, string) {
	role := strings.ToLower(strings.TrimSpace(r.Header.Get("x-panel-role")))
	if role != "admin" {
		return false, "admin-required"
	}

	secret := strings.TrimSpace(a.panelHMAC)
	if secret == "" {
		secret = strings.TrimSpace(a.token)
	}
	if secret == "" {
		return false, "hmac-not-configured"
	}

	ts := r.Header.Get("x-panel-ts")
	sig := r.Header.Get("x-panel-sign")
	if ts == "" || sig == "" {
		return false, "missing-signature"
	}

	tsNum, err := strconv.ParseInt(strings.TrimSpace(ts), 10, 64)
	if err != nil {
		return false, "expired"
	}
	now := time.Now().UnixMilli()
	drift := now - tsNum
	if drift < 0 {
		drift = -drift
	}
	if drift > 5*60*1000 {
		return false, "expired"
	}

	base := fmt.Sprintf("%s|%s|%s|%s|%s", strings.TrimSpace(name), "reinstall", template, role, ts)
	m := hmac.New(sha256.New, []byte(secret))
	_, _ = m.Write([]byte(base))
	expect := hex.EncodeToString(m.Sum(nil))
	if !timingSafeEq(strings.TrimSpace(sig), expect) {
		return false, "bad-signature"
	}
	return true, ""
}

func (a *Agent) nodeInformationPath() string {
	return filepath.Join(a.dataRoot, NodeInfoFileName)
}

func (a *Agent) defaultNodeInformation() NodeInformation {
	return NodeInformation{
		NodeVersion:      normalizeNodeVersionTag(NodeVersion),
		NodeArchitecture: runtime.GOOS + "/" + runtime.GOARCH,
		NodeAutoUpdate:   false,
	}
}

func (a *Agent) readNodeInformation() NodeInformation {
	info := a.defaultNodeInformation()
	b, err := os.ReadFile(a.nodeInformationPath())
	if err != nil || len(bytes.TrimSpace(b)) == 0 {
		return info
	}

	var arr []NodeInformation
	if err := json.Unmarshal(b, &arr); err == nil && len(arr) > 0 {
		if arr[0].NodeVersion != "" {
			info.NodeVersion = normalizeNodeVersionTag(arr[0].NodeVersion)
		}
		if arr[0].NodeArchitecture != "" {
			info.NodeArchitecture = arr[0].NodeArchitecture
		}
		info.NodeAutoUpdate = arr[0].NodeAutoUpdate
		info.UpdatedAt = arr[0].UpdatedAt
		return info
	}

	var obj NodeInformation
	if err := json.Unmarshal(b, &obj); err == nil {
		if obj.NodeVersion != "" {
			info.NodeVersion = normalizeNodeVersionTag(obj.NodeVersion)
		}
		if obj.NodeArchitecture != "" {
			info.NodeArchitecture = obj.NodeArchitecture
		}
		info.NodeAutoUpdate = obj.NodeAutoUpdate
		info.UpdatedAt = obj.UpdatedAt
	}
	return info
}

func (a *Agent) writeNodeInformation(info NodeInformation) error {
	if err := ensureDir(a.dataRoot); err != nil {
		return err
	}
	if info.NodeVersion == "" {
		info.NodeVersion = normalizeNodeVersionTag(NodeVersion)
	} else {
		info.NodeVersion = normalizeNodeVersionTag(info.NodeVersion)
	}
	if info.NodeArchitecture == "" {
		info.NodeArchitecture = runtime.GOOS + "/" + runtime.GOARCH
	}
	info.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	b, err := json.MarshalIndent([]NodeInformation{info}, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return writeFileNoFollow(a.nodeInformationPath(), b, 0o644)
}

func (a *Agent) ensureNodeInformationFile() NodeInformation {
	info := a.readNodeInformation()
	if _, err := os.Stat(a.nodeInformationPath()); err != nil {
		if writeErr := a.writeNodeInformation(info); writeErr != nil {
			fmt.Fprintf(os.Stderr, "[node-update] failed to initialize %s: %v\n", NodeInfoFileName, writeErr)
		}
	}
	return info
}

func (a *Agent) verifyNodeAdminRequest(r *http.Request, action, subject string) (bool, string) {
	role := strings.ToLower(strings.TrimSpace(r.Header.Get("x-panel-role")))
	if role != "admin" {
		return false, "admin-required"
	}

	secret := strings.TrimSpace(a.panelHMAC)
	if secret == "" {
		secret = strings.TrimSpace(a.token)
	}
	if secret == "" {
		return false, "hmac-not-configured"
	}

	ts := r.Header.Get("x-panel-ts")
	sig := r.Header.Get("x-panel-sign")
	if ts == "" || sig == "" {
		return false, "missing-signature"
	}

	tsNum, err := strconv.ParseInt(strings.TrimSpace(ts), 10, 64)
	if err != nil {
		return false, "expired"
	}
	now := time.Now().UnixMilli()
	drift := now - tsNum
	if drift < 0 {
		drift = -drift
	}
	if drift > 5*60*1000 {
		return false, "expired"
	}

	base := fmt.Sprintf("%s|%s|%s|%s|%s", strings.TrimSpace(a.uuid), strings.TrimSpace(action), strings.TrimSpace(subject), role, ts)
	m := hmac.New(sha256.New, []byte(secret))
	_, _ = m.Write([]byte(base))
	expect := hex.EncodeToString(m.Sum(nil))
	if !timingSafeEq(strings.TrimSpace(sig), expect) {
		return false, "bad-signature"
	}
	return true, ""
}

func (a *Agent) writeNodeUpdateFile(rel string, data []byte, backupDir string) error {
	if !allowedNodeUpdateFile(rel) {
		return fmt.Errorf("file is not allowed: %s", rel)
	}
	if err := validateNodeUpdateFileContent(rel, data); err != nil {
		return err
	}
	if err := ensureDir(a.dataRoot); err != nil {
		return err
	}

	target := filepath.Join(a.dataRoot, rel)
	resolvedRoot, err := filepath.Abs(a.dataRoot)
	if err != nil {
		return err
	}
	resolvedTarget, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	if resolvedTarget != resolvedRoot && !strings.HasPrefix(resolvedTarget, resolvedRoot+string(filepath.Separator)) {
		return fmt.Errorf("refusing path outside node root: %s", rel)
	}

	if st, err := os.Lstat(target); err == nil && st.Mode().IsRegular() && backupDir != "" {
		if err := copyRegularFile(target, filepath.Join(backupDir, rel)); err != nil {
			return fmt.Errorf("backup failed for %s: %w", rel, err)
		}
	}

	tmp := filepath.Join(a.dataRoot, fmt.Sprintf(".%s.tmp.%d", rel, time.Now().UnixNano()))
	if err := writeFileNoFollow(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (a *Agent) runGoClean() error {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "clean")
	cmd.Dir = a.dataRoot
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("go clean timed out")
	}
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 400 {
			msg = msg[:400]
		}
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("go clean failed: %s", msg)
	}
	return nil
}

func (a *Agent) scheduleAdaemonRestart() {
	go func() {
		time.Sleep(900 * time.Millisecond)
		cmd := exec.Command("systemctl", "restart", "adaemon")
		out, err := cmd.CombinedOutput()
		if err != nil {
			msg := strings.TrimSpace(string(out))
			if len(msg) > 400 {
				msg = msg[:400]
			}
			fmt.Fprintf(os.Stderr, "[node-update] systemctl restart adaemon failed: %v %s\n", err, msg)
			return
		}
		fmt.Println("[node-update] systemctl restart adaemon executed")
	}()
}

func (a *Agent) handleNodeUpdateInfo(w http.ResponseWriter, r *http.Request) {
	info := a.ensureNodeInformationFile()
	jsonWrite(w, 200, map[string]any{
		"ok":                true,
		"version":           info.NodeVersion,
		"architecture":      info.NodeArchitecture,
		"autoUpdateEnabled": info.NodeAutoUpdate,
		"nodeId":            a.uuid,
	})
}

func (a *Agent) handleNodeUpdateAuto(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1024*1024)
	var payload nodeUpdateAutoRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil && !errors.Is(err, io.EOF) {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid request body"})
		return
	}
	subject := "off"
	if payload.Enabled {
		subject = "on"
	}
	if ok, reason := a.verifyNodeAdminRequest(r, "node-update-auto", subject); !ok {
		jsonWrite(w, 403, map[string]any{"ok": false, "error": reason})
		return
	}
	info := a.readNodeInformation()
	info.NodeAutoUpdate = payload.Enabled
	if err := a.writeNodeInformation(info); err != nil {
		jsonWrite(w, 500, map[string]any{"ok": false, "error": "failed to save node information"})
		return
	}
	jsonWrite(w, 200, map[string]any{"ok": true, "autoUpdateEnabled": payload.Enabled})
}

func (a *Agent) handleNodeUpdateInstall(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, NodeUpdateMaxBody)
	var payload nodeUpdateInstallRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid request body"})
		return
	}

	version := normalizeNodeVersionTag(payload.Version)
	if !validNodeUpdateVersion(version) {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid version"})
		return
	}
	if ok, reason := a.verifyNodeAdminRequest(r, "node-update-install", version); !ok {
		jsonWrite(w, 403, map[string]any{"ok": false, "error": reason})
		return
	}
	if len(payload.Files) == 0 || len(payload.Files) > 32 {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid update file set"})
		return
	}

	seenMain := false
	totalBytes := 0
	decoded := make(map[string][]byte, len(payload.Files))
	for _, file := range payload.Files {
		rel := strings.TrimSpace(file.Path)
		if !allowedNodeUpdateFile(rel) {
			jsonWrite(w, 400, map[string]any{"ok": false, "error": "update contains an unsupported file"})
			return
		}
		if _, exists := decoded[rel]; exists {
			jsonWrite(w, 400, map[string]any{"ok": false, "error": "duplicate update file"})
			return
		}
		data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(file.ContentBase64))
		if err != nil {
			jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid update file encoding"})
			return
		}
		if err := validateNodeUpdateFileContent(rel, data); err != nil {
			jsonWrite(w, 400, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		totalBytes += len(data)
		if totalBytes > NodeUpdateMaxBody {
			jsonWrite(w, 413, map[string]any{"ok": false, "error": "update payload too large"})
			return
		}
		if rel == "main.go" {
			seenMain = true
		}
		decoded[rel] = data
	}
	if !seenMain {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "main.go is required"})
		return
	}

	backupDir := filepath.Join(a.dataRoot, ".update-backups", time.Now().UTC().Format("20060102-150405"))
	filesUpdated := 0
	for rel, data := range decoded {
		if err := a.writeNodeUpdateFile(rel, data, backupDir); err != nil {
			jsonWrite(w, 500, map[string]any{"ok": false, "error": "failed to write update file"})
			return
		}
		filesUpdated++
	}

	info := a.readNodeInformation()
	info.NodeVersion = version
	info.NodeArchitecture = runtime.GOOS + "/" + runtime.GOARCH
	if err := a.writeNodeInformation(info); err != nil {
		jsonWrite(w, 500, map[string]any{"ok": false, "error": "failed to save node information"})
		return
	}

	if err := a.runGoClean(); err != nil {
		jsonWrite(w, 500, map[string]any{"ok": false, "error": err.Error(), "filesUpdated": filesUpdated})
		return
	}

	a.scheduleAdaemonRestart()
	jsonWrite(w, 200, map[string]any{
		"ok":             true,
		"version":        version,
		"filesUpdated":   filesUpdated,
		"restartCommand": "systemctl restart adaemon",
		"message":        "Node update installed. go clean completed and adaemon restart was scheduled.",
	})
}

const (
	wsMaxSubscribers    = 5
	wsStatusIntervalSec = 10
	wsWriteTimeout      = 5 * time.Second
	wsReadTimeout       = 60 * time.Second
)

type wsSubscriber struct {
	msgs chan []byte
	done chan struct{}
}

func (a *Agent) collectServerStatuses() map[string]any {
	entries, err := os.ReadDir(a.volumesDir)
	if err != nil {
		return map[string]any{"ok": false, "error": "read volumes"}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var servers []ServerStatus
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		name := e.Name()
		serverDir := a.serverDir(name)

		ss := ServerStatus{Name: name, Status: "stopped"}

		meta := a.loadMeta(serverDir)
		cpuCoresConfig := metaCpuCores(meta)
		_, cpuLimit := computeCpuLimit(cpuCoresConfig)
		ss.CpuLimit = &cpuLimit

		if a.docker.containerExists(ctx, name) {
			out, _, _, err := a.docker.runCollect(ctx, "inspect", "-f", "{{.State.Running}}", name)
			if err == nil && strings.TrimSpace(out) == "true" {
				ss.Status = "running"

				startedAt, _, _, startErr := a.docker.runCollect(ctx, "inspect", "-f", "{{.State.StartedAt}}", name)
				if startErr == nil && strings.TrimSpace(startedAt) != "" {
					startTime, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(startedAt))
					if parseErr == nil {
						uptime := int64(time.Since(startTime).Seconds())
						ss.Uptime = &uptime
					}
				}

				statsOut, _, _, statsErr := a.docker.runCollect(ctx, "stats", "--no-stream", "--format",
					"{{.CPUPerc}}|{{.MemUsage}}", name)
				if statsErr == nil && strings.TrimSpace(statsOut) != "" {
					parts := strings.Split(strings.TrimSpace(statsOut), "|")
					if len(parts) >= 2 {
						cpuStr := strings.TrimSuffix(strings.TrimSpace(parts[0]), "%")
						if cpu, err := strconv.ParseFloat(cpuStr, 64); err == nil {
							if cpu > cpuLimit {
								cpu = cpuLimit
							}
							ss.CPU = &cpu
						}
						memParts := strings.Split(parts[1], "/")
						if len(memParts) >= 1 {
							memMb := parseMemoryString(strings.TrimSpace(memParts[0]))
							if memMb > 0 {
								ss.Memory = &memMb
							}
						}
					}
				}
			}
		}

		diskBytes := a.getServerDiskUsage(serverDir)
		if diskBytes > 0 {
			diskMb := float64(diskBytes) / (1024 * 1024)
			ss.Disk = &diskMb
		}

		servers = append(servers, ss)
	}

	return map[string]any{
		"type":      "status",
		"ok":        true,
		"nodeId":    a.uuid,
		"timestamp": time.Now().UnixMilli(),
		"servers":   servers,
		"resources": map[string]any{
			"cpuCount": runtime.NumCPU(),
		},
	}
}

func (a *Agent) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		if a.debug {
			fmt.Printf("[ws] accept error: %v\n", err)
		}
		return
	}

	sub := &wsSubscriber{
		msgs: make(chan []byte, 4),
		done: make(chan struct{}),
	}

	a.wsMu.Lock()
	if a.wsSubscribers == nil {
		a.wsSubscribers = make(map[*wsSubscriber]struct{})
	}
	if len(a.wsSubscribers) >= wsMaxSubscribers {
		a.wsMu.Unlock()
		conn.Close(websocket.StatusTryAgainLater, "too many connections")
		return
	}
	a.wsSubscribers[sub] = struct{}{}
	a.wsMu.Unlock()

	defer func() {
		conn.Close(websocket.StatusNormalClosure, "closing")
		a.wsMu.Lock()
		delete(a.wsSubscribers, sub)
		a.wsMu.Unlock()
		close(sub.done)
	}()

	fmt.Printf("[ws] panel connected (subscribers: %d)\n", func() int {
		a.wsMu.Lock()
		defer a.wsMu.Unlock()
		return len(a.wsSubscribers)
	}())

	snapshot := a.collectServerStatuses()
	ctx, cancel := context.WithTimeout(r.Context(), wsWriteTimeout)
	_ = wsjson.Write(ctx, conn, snapshot)
	cancel()

	go func() {
		for {
			_, _, err := conn.Read(r.Context())
			if err != nil {
				return
			}
		}
	}()

	ticker := time.NewTicker(wsStatusIntervalSec * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			fmt.Printf("[ws] panel disconnected\n")
			return
		case <-ticker.C:
			status := a.collectServerStatuses()
			data, err := json.Marshal(status)
			if err != nil {
				continue
			}
			writeCtx, writeCancel := context.WithTimeout(r.Context(), wsWriteTimeout)
			err = conn.Write(writeCtx, websocket.MessageText, data)
			writeCancel()
			if err != nil {
				if a.debug {
					fmt.Printf("[ws] write error: %v\n", err)
				}
				return
			}
		case msg := <-sub.msgs:
			writeCtx, writeCancel := context.WithTimeout(r.Context(), wsWriteTimeout)
			err := conn.Write(writeCtx, websocket.MessageText, msg)
			writeCancel()
			if err != nil {
				if a.debug {
					fmt.Printf("[ws] write error: %v\n", err)
				}
				return
			}
		}
	}
}

func (a *Agent) broadcastContainerEvent(name, status string) {
	a.wsMu.Lock()
	subs := make([]*wsSubscriber, 0, len(a.wsSubscribers))
	for s := range a.wsSubscribers {
		subs = append(subs, s)
	}
	a.wsMu.Unlock()

	if len(subs) == 0 {
		return
	}

	evt := map[string]any{
		"type":      "event",
		"nodeId":    a.uuid,
		"timestamp": time.Now().UnixMilli(),
		"server":    name,
		"status":    status,
	}
	data, err := json.Marshal(evt)
	if err != nil {
		return
	}

	for _, s := range subs {
		select {
		case s.msgs <- data:
		default:
		}
	}
}

func statUIDGID(p string) (uid, gid int) {
	uid, gid = 1000, 1000
	st, err := os.Stat(p)
	if err != nil {
		return
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	return int(sys.Uid), int(sys.Gid)
}

const (
	cpuPeriod        = 100000
	defaultPidsLimit = 512
	minIoWeight      = 10
	maxIoWeight      = 1000
	minCpuWeight     = 1
	maxCpuWeight     = 1000
	minPidsLimit     = 64
	maxPidsLimit     = 4096
	minFileLimit     = 1024
	maxFileLimit     = 1048576

	minNetworkLimitMb       int64 = 1
	maxNetworkLimitMb       int64 = 1073741824
	defaultNetworkResetMins       = 1440
	minNetworkResetMins           = 1
	maxNetworkResetMins           = 43200
)

func dockerCpuSharesFromWeight(weight int) int64 {
	if weight <= 0 {
		return 0
	}
	if weight < minCpuWeight {
		weight = minCpuWeight
	}
	if weight > maxCpuWeight {
		weight = maxCpuWeight
	}
	shares := int64((weight * 2048) / maxCpuWeight)
	if shares < 2 {
		return 2
	}
	return shares
}

func runtimeNeedsHighFileLimit(tpl, imageRef string) bool {
	tpl = strings.ToLower(strings.TrimSpace(tpl))
	imageRef = strings.ToLower(strings.TrimSpace(imageRef))
	if tpl == "rust" {
		return true
	}
	return strings.Contains(imageRef, "pterodactyl/games:rust") ||
		strings.Contains(imageRef, "parkervcp/games:rust") ||
		strings.Contains(imageRef, "didstopia/rust-server")
}

func withRuntimeDefaultFileLimit(resources *resourceLimits, tpl, imageRef string) *resourceLimits {
	if !runtimeNeedsHighFileLimit(tpl, imageRef) {
		return resources
	}
	if resources != nil && resources.FileLimit != nil {
		return resources
	}

	limit := maxFileLimit
	if resources == nil {
		return &resourceLimits{FileLimit: &limit}
	}
	copy := *resources
	copy.FileLimit = &limit
	return &copy
}

const rustPterodactylStartup = `./RustDedicated -batchmode -nographics -logfile "{{LOG_FILE}}" +server.ip 0.0.0.0 +server.port {{SERVER_PORT}} +server.queryport {{QUERY_PORT}} +server.identity "{{SERVER_IDENTITY}}" +server.level "{{SERVER_LEVEL}}" +server.seed {{SERVER_SEED}} +server.worldsize {{WORLD_SIZE}} +server.hostname "{{SERVER_NAME}}" +server.description "{{SERVER_DESCRIPTION}}" +server.maxplayers {{MAX_PLAYERS}} +server.secure {{SERVER_SECURE}} +rcon.ip 0.0.0.0 +rcon.web {{RCON_WEB}} +rcon.port {{RCON_PORT}} +rcon.password "{{RCON_PASS}}" +app.port {{RUST_PLUS_PORT}} +server.saveinterval {{SAVE_INTERVAL}} {{ADDITIONAL_ARGS}}`

func normalizeRustRuntimeForPterodactyl(meta map[string]any) bool {
	if meta == nil {
		return false
	}
	template := strings.ToLower(strings.TrimSpace(fmt.Sprint(meta["type"])))
	if template == "" || template == "<nil>" {
		template = strings.ToLower(strings.TrimSpace(fmt.Sprint(meta["template"])))
	}
	runtimeObj, _ := meta["runtime"].(map[string]any)
	image := ""
	tag := ""
	if runtimeObj != nil {
		image = strings.ToLower(strings.TrimSpace(fmt.Sprint(runtimeObj["image"])))
		tag = strings.ToLower(strings.TrimSpace(fmt.Sprint(runtimeObj["tag"])))
	}
	imageRef := image + ":" + tag
	isRust := template == "rust" ||
		strings.Contains(imageRef, "didstopia/rust-server") ||
		strings.Contains(imageRef, "pterodactyl/games:rust") ||
		strings.Contains(imageRef, "parkervcp/games:rust")
	if !isRust {
		return false
	}
	if runtimeObj == nil {
		runtimeObj = map[string]any{}
	}

	changed := false
	set := func(key string, value any) {
		if !reflect.DeepEqual(runtimeObj[key], value) {
			runtimeObj[key] = value
			changed = true
		}
	}
	set("image", "ghcr.io/pterodactyl/games")
	set("tag", "rust")
	set("volumes", []any{"{BOT_DIR}:/home/container"})
	set("workdir", "/home/container")
	set("command", "")
	set("adpanelStartup", "")
	set("startupDisplay", "/bin/bash /entrypoint.sh")
	set("ports", []any{28015, 28016, 28082})
	set("portProtocols", map[string]any{
		"28015": []any{"udp"},
		"28016": []any{"tcp", "udp"},
		"28082": []any{"tcp", "udp"},
	})
	set("console", map[string]any{
		"type":            "stdin",
		"targetProcesses": []any{"RustDedicated"},
	})

	env := runtimeEnvMap(runtimeObj["env"])
	if env == nil {
		env = map[string]string{}
	}
	copyEnv := func(dst, src string) {
		if strings.TrimSpace(env[dst]) == "" && strings.TrimSpace(env[src]) != "" {
			env[dst] = env[src]
			changed = true
		}
	}
	copyEnv("SERVER_IDENTITY", "RUST_SERVER_IDENTITY")
	copyEnv("SERVER_PORT", "RUST_SERVER_PORT")
	copyEnv("QUERY_PORT", "RUST_SERVER_QUERYPORT")
	copyEnv("SERVER_SEED", "RUST_SERVER_SEED")
	copyEnv("WORLD_SIZE", "RUST_SERVER_WORLDSIZE")
	copyEnv("SERVER_NAME", "RUST_SERVER_NAME")
	copyEnv("SERVER_DESCRIPTION", "RUST_SERVER_DESCRIPTION")
	copyEnv("MAX_PLAYERS", "RUST_SERVER_MAXPLAYERS")
	copyEnv("RCON_PORT", "RUST_RCON_PORT")
	copyEnv("RCON_PASS", "RUST_RCON_PASSWORD")
	copyEnv("RUST_PLUS_PORT", "RUST_APP_PORT")

	envDefaults := map[string]string{
		"STARTUP":            rustPterodactylStartup,
		"AUTO_UPDATE":        "0",
		"FRAMEWORK":          "",
		"OXIDE":              "0",
		"LD_LIBRARY_PATH":    "/home/container/RustDedicated_Data/Plugins/x86_64:/home/container",
		"LOG_FILE":           "/home/container/logs/rust.log",
		"SERVER_PORT":        "28015",
		"QUERY_PORT":         "28016",
		"RCON_PORT":          "28016",
		"RUST_PLUS_PORT":     "28082",
		"SERVER_IDENTITY":    "adpanel",
		"SERVER_LEVEL":       "Procedural Map",
		"SERVER_SEED":        "12345",
		"WORLD_SIZE":         "3500",
		"SERVER_NAME":        "ADPanel Rust Server",
		"SERVER_DESCRIPTION": "Rust server hosted with ADPanel",
		"MAX_PLAYERS":        "100",
		"SERVER_SECURE":      "1",
		"RCON_WEB":           "true",
		"RCON_PASS":          "change-me",
		"RCON_PASSWORD":      "change-me",
		"RCON_IP":            "127.0.0.1",
		"SAVE_INTERVAL":      "300",
		"ADDITIONAL_ARGS":    "",
		"TZ":                 "Etc/UTC",
	}
	for key, value := range envDefaults {
		if _, ok := env[key]; !ok {
			env[key] = value
			changed = true
		}
	}
	if env["RCON_PASSWORD"] != env["RCON_PASS"] {
		env["RCON_PASSWORD"] = env["RCON_PASS"]
		changed = true
	}
	if !reflect.DeepEqual(runtimeObj["env"], env) {
		runtimeObj["env"] = env
		changed = true
	}

	if meta["type"] != "rust" {
		meta["type"] = "rust"
		changed = true
	}
	if meta["template"] != "rust" {
		meta["template"] = "rust"
		changed = true
	}
	if !reflect.DeepEqual(meta["runtime"], runtimeObj) {
		meta["runtime"] = runtimeObj
		changed = true
	}
	return changed
}

func validateResourcePerformanceLimits(resources *resourceLimits) string {
	if resources == nil {
		return ""
	}
	if resources.IoWeight != nil && (*resources.IoWeight < minIoWeight || *resources.IoWeight > maxIoWeight) {
		return fmt.Sprintf("I/O Priority must be between %d and %d.", minIoWeight, maxIoWeight)
	}
	if resources.CpuWeight != nil && (*resources.CpuWeight < minCpuWeight || *resources.CpuWeight > maxCpuWeight) {
		return fmt.Sprintf("CPU Priority must be between %d and %d.", minCpuWeight, maxCpuWeight)
	}
	if resources.PidsLimit != nil && (*resources.PidsLimit < minPidsLimit || *resources.PidsLimit > maxPidsLimit) {
		return fmt.Sprintf("Process Limit must be between %d and %d.", minPidsLimit, maxPidsLimit)
	}
	if resources.FileLimit != nil && (*resources.FileLimit < minFileLimit || *resources.FileLimit > maxFileLimit) {
		return fmt.Sprintf("File Limit must be between %d and %d.", minFileLimit, maxFileLimit)
	}
	if resources.NetworkInboundLimitMb != nil && *resources.NetworkInboundLimitMb != 0 && (*resources.NetworkInboundLimitMb < minNetworkLimitMb || *resources.NetworkInboundLimitMb > maxNetworkLimitMb) {
		return fmt.Sprintf("Inbound Limit must be between %d and %d.", minNetworkLimitMb, maxNetworkLimitMb)
	}
	if resources.NetworkOutboundLimitMb != nil && *resources.NetworkOutboundLimitMb != 0 && (*resources.NetworkOutboundLimitMb < minNetworkLimitMb || *resources.NetworkOutboundLimitMb > maxNetworkLimitMb) {
		return fmt.Sprintf("Outbound Limit must be between %d and %d.", minNetworkLimitMb, maxNetworkLimitMb)
	}
	if resources.NetworkResetMinutes != nil && *resources.NetworkResetMinutes != 0 && (*resources.NetworkResetMinutes < minNetworkResetMins || *resources.NetworkResetMinutes > maxNetworkResetMins) {
		return fmt.Sprintf("Reset Time must be between %d and %d.", minNetworkResetMins, maxNetworkResetMins)
	}
	return ""
}

func computeCpuLimit(allocatedCores float64) (effectiveCores, maxPercent float64) {
	hostCores := float64(runtime.NumCPU())
	if hostCores <= 0 {
		hostCores = 1
	}
	effectiveCores = hostCores
	if allocatedCores > 0 {
		effectiveCores = allocatedCores
		if effectiveCores > hostCores {
			effectiveCores = hostCores
		}
	}
	maxPercent = effectiveCores * 100
	return
}

func metaCpuCores(meta map[string]any) float64 {
	if meta == nil {
		return 0
	}
	res, ok := meta["resources"].(map[string]any)
	if !ok {
		return 0
	}
	switch v := res["cpuCores"].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return 0
}

const (
	defaultMinecraftProcessImage   = "eclipse-temurin"
	defaultMinecraftProcessTag     = "21-jre"
	defaultMinecraftProcessCommand = "java -Xms128M -Xmx{RAM_MB}M -jar /data/server.jar nogui"
	maxRuntimeCommandLen           = 4000
)

var (
	fallbackMinecraftJavaVersions      = []string{"8", "11", "17", "21", "22", "23", "24", "25", "26"}
	minecraftJavaVersionsCache         []string
	minecraftJavaVersionsCacheExpireAt time.Time
	minecraftJavaVersionsMu            sync.Mutex
)

const (
	minecraftJavaMinVersion       = 8
	minecraftJavaMaxVersion       = 99
	minecraftJavaDiscoveryURL     = "https://hub.docker.com/v2/repositories/library/eclipse-temurin/tags?page_size=100&name=-jre"
	minecraftJavaDiscoveryTimeout = 4500 * time.Millisecond
	minecraftJavaDiscoveryTTL     = 6 * time.Hour
	minecraftJavaDiscoveryFailTTL = 5 * time.Minute
)

type dockerHubTagsResponse struct {
	Next    string `json:"next"`
	Results []struct {
		Name string `json:"name"`
	} `json:"results"`
}

func normalizeMinecraftJavaVersion(raw any) string {
	value := strings.TrimSpace(strings.ToLower(fmt.Sprint(raw)))
	if value == "" || value == "<nil>" {
		return ""
	}
	var digits strings.Builder
	for _, r := range value {
		if r < '0' || r > '9' {
			break
		}
		digits.WriteRune(r)
	}
	version := digits.String()
	if version == "" {
		return ""
	}
	parsed, err := strconv.Atoi(version)
	if err != nil || parsed < minecraftJavaMinVersion || parsed > minecraftJavaMaxVersion {
		return ""
	}
	return fmt.Sprint(parsed)
}

func minecraftJavaTag(version any) string {
	clean := normalizeMinecraftJavaVersion(version)
	if clean == "" {
		return defaultMinecraftProcessTag
	}
	return clean + "-jre"
}

func minecraftJavaVersionFromTag(raw any) string {
	tag := strings.TrimSpace(strings.ToLower(fmt.Sprint(raw)))
	if tag == "" || tag == "<nil>" {
		return ""
	}
	if idx := strings.LastIndex(tag, ":"); idx >= 0 && idx+1 < len(tag) {
		tag = tag[idx+1:]
	}
	return normalizeMinecraftJavaVersion(tag)
}

func cloneStringSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}

func mergeMinecraftJavaVersions(discovered []string) []string {
	seen := make(map[int]struct{})
	for _, raw := range fallbackMinecraftJavaVersions {
		if clean := normalizeMinecraftJavaVersion(raw); clean != "" {
			if n, err := strconv.Atoi(clean); err == nil {
				seen[n] = struct{}{}
			}
		}
	}
	for _, raw := range discovered {
		if clean := normalizeMinecraftJavaVersion(raw); clean != "" {
			if n, err := strconv.Atoi(clean); err == nil {
				seen[n] = struct{}{}
			}
		}
	}
	nums := make([]int, 0, len(seen))
	for n := range seen {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	out := make([]string, 0, len(nums))
	for _, n := range nums {
		out = append(out, fmt.Sprint(n))
	}
	return out
}

func extractMinecraftJavaVersionsFromDockerHubPayload(payload dockerHubTagsResponse) []string {
	versions := []string{}
	for _, result := range payload.Results {
		name := strings.TrimSpace(strings.ToLower(result.Name))
		if !strings.HasSuffix(name, "-jre") {
			continue
		}
		prefix := strings.TrimSuffix(name, "-jre")
		if prefix == "" || strings.ContainsAny(prefix, ".-_") {
			continue
		}
		if version := normalizeMinecraftJavaVersion(prefix); version != "" {
			versions = append(versions, version)
		}
	}
	return versions
}

func discoverMinecraftJavaVersions(ctx context.Context) ([]string, bool) {
	discovered := []string{}
	nextURL := minecraftJavaDiscoveryURL
	client := &http.Client{Timeout: minecraftJavaDiscoveryTimeout}
	ok := false
	for page := 0; page < 5 && nextURL != ""; page++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, nextURL, nil)
		if err != nil {
			break
		}
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			break
		}
		func() {
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				nextURL = ""
				return
			}
			ok = true
			var payload dockerHubTagsResponse
			if err := json.NewDecoder(io.LimitReader(resp.Body, 2*1024*1024)).Decode(&payload); err != nil {
				nextURL = ""
				return
			}
			discovered = append(discovered, extractMinecraftJavaVersionsFromDockerHubPayload(payload)...)
			next := strings.TrimSpace(payload.Next)
			if next == "" || !strings.HasPrefix(next, "https://hub.docker.com/v2/repositories/library/eclipse-temurin/tags") {
				nextURL = ""
				return
			}
			nextURL = next
		}()
	}
	return mergeMinecraftJavaVersions(discovered), ok
}

func minecraftJavaVersions() []string {
	now := time.Now()
	minecraftJavaVersionsMu.Lock()
	if len(minecraftJavaVersionsCache) > 0 && now.Before(minecraftJavaVersionsCacheExpireAt) {
		cached := cloneStringSlice(minecraftJavaVersionsCache)
		minecraftJavaVersionsMu.Unlock()
		return cached
	}
	minecraftJavaVersionsMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), minecraftJavaDiscoveryTimeout)
	discovered, ok := discoverMinecraftJavaVersions(ctx)
	cancel()
	if len(discovered) == 0 {
		discovered = mergeMinecraftJavaVersions(nil)
	}

	minecraftJavaVersionsMu.Lock()
	minecraftJavaVersionsCache = cloneStringSlice(discovered)
	if ok {
		minecraftJavaVersionsCacheExpireAt = time.Now().Add(minecraftJavaDiscoveryTTL)
	} else {
		minecraftJavaVersionsCacheExpireAt = time.Now().Add(minecraftJavaDiscoveryFailTTL)
	}
	out := cloneStringSlice(minecraftJavaVersionsCache)
	minecraftJavaVersionsMu.Unlock()
	return out
}

func minecraftJavaOptionsPayload(selected string) map[string]any {
	selected = normalizeMinecraftJavaVersion(selected)
	if selected == "" {
		selected = minecraftJavaVersionFromTag(defaultMinecraftProcessTag)
	}
	if selected == "" {
		selected = "21"
	}
	versions := minecraftJavaVersions()
	hasSelected := false
	for _, version := range versions {
		if version == selected {
			hasSelected = true
			break
		}
	}
	if !hasSelected {
		versions = mergeMinecraftJavaVersions(append(versions, selected))
	}
	options := make([]map[string]string, 0, len(versions))
	for _, version := range versions {
		options = append(options, map[string]string{
			"version": version,
			"tag":     minecraftJavaTag(version),
			"label":   "Java " + version,
		})
	}
	return map[string]any{
		"selected": selected,
		"image":    defaultMinecraftProcessImage,
		"tag":      minecraftJavaTag(selected),
		"options":  options,
	}
}

func sanitizeRuntimeCommand(raw any) string {
	s := strings.ReplaceAll(fmt.Sprint(raw), "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.TrimSpace(s)
	if len(s) > maxRuntimeCommandLen {
		s = s[:maxRuntimeCommandLen]
	}
	if s == "<nil>" {
		return ""
	}
	return s
}

func stripLegacyStartupCommandFields(meta map[string]any) bool {
	if meta == nil {
		return false
	}
	changed := false
	if _, ok := meta["startupCommand"]; ok {
		delete(meta, "startupCommand")
		changed = true
	}
	if runtimeObj, ok := meta["runtime"].(map[string]any); ok {
		if _, ok := runtimeObj["startupCommand"]; ok {
			delete(runtimeObj, "startupCommand")
			meta["runtime"] = runtimeObj
			changed = true
		}
	}
	return changed
}

func defaultStartFileForType(tpl string) string {
	switch strings.ToLower(strings.TrimSpace(tpl)) {
	case "minecraft":
		return "server.jar"
	case "python":
		return "main.py"
	case "nodejs", "discord-bot":
		return "index.js"
	default:
		return ""
	}
}

func defaultDataDirForType(tpl string) string {
	switch strings.ToLower(strings.TrimSpace(tpl)) {
	case "python", "nodejs", "discord-bot", "runtime":
		return "/app"
	case "fivem", "five-m", "redm":
		return cfxRuntimeServerDir
	default:
		return "/data"
	}
}

func hasManagedRuntimeDefaults(tpl string) bool {
	switch strings.ToLower(strings.TrimSpace(tpl)) {
	case "minecraft", "python", "nodejs", "discord-bot", "runtime":
		return true
	default:
		return false
	}
}

func isManagedProcessRuntime(tpl string) bool {
	switch strings.ToLower(strings.TrimSpace(tpl)) {
	case "python", "nodejs", "discord-bot", "runtime":
		return true
	default:
		return false
	}
}

func defaultRuntimeCommandForType(tpl, startFile, dataDir string) string {
	switch strings.ToLower(strings.TrimSpace(tpl)) {
	case "minecraft":
		return defaultMinecraftProcessCommand
	case "python":
		if startFile == "" {
			startFile = "main.py"
		}
		return fmt.Sprintf("python %s/%s", strings.TrimRight(dataDir, "/"), startFile)
	case "nodejs", "discord-bot":
		if startFile == "" {
			startFile = "index.js"
		}
		return fmt.Sprintf("node %s/%s", strings.TrimRight(dataDir, "/"), startFile)
	default:
		return ""
	}
}

func (a *Agent) inferRuntimeProcessCommandFromImage(ctx context.Context, tpl, imageRef, startFile, dataDir, fallbackCommand string) (string, string, imageConfig, error) {
	cleanTemplate := strings.ToLower(strings.TrimSpace(tpl))
	cleanImageRef := strings.TrimSpace(imageRef)
	cleanStartFile := strings.TrimSpace(startFile)
	providedDataDir := strings.TrimSpace(dataDir) != ""
	cleanDataDir := strings.TrimSpace(dataDir)
	if cleanStartFile == "" {
		cleanStartFile = defaultStartFileForType(cleanTemplate)
	}
	if cleanDataDir == "" {
		cleanDataDir = defaultDataDirForType(cleanTemplate)
	}

	fallback := sanitizeRuntimeCommand(fallbackCommand)
	if fallback == "" {
		fallback = defaultRuntimeCommandForType(cleanTemplate, cleanStartFile, cleanDataDir)
	}
	inspected := imageConfig{Env: map[string]string{}}

	if cleanImageRef != "" {
		if !a.docker.imageExistsLocally(ctx, cleanImageRef) {
			a.docker.pull(ctx, cleanImageRef)
		}

		imgCfg := a.docker.inspectImageConfig(ctx, cleanImageRef)
		inspected = imgCfg
		if !providedDataDir {
			if wd := strings.TrimSpace(imgCfg.Workdir); wd != "" {
				cleanDataDir = wd
			}
		}

		candidate := runtimeCommandFromImageConfig(imgCfg)
		if candidate != "" {
			candidate = normalizeLegacyRuntimeProcessCommand(cleanTemplate, candidate, cleanStartFile, cleanDataDir)
			if shouldPreferImageDefaultRuntimeCommand(candidate, imgCfg) {
				return candidate, "image-default", inspected, nil
			}
			if reason := validateRuntimeProcessCommand(candidate); reason == "" {
				return candidate, "image-inspect", inspected, nil
			}
		}
	}

	fallback = normalizeLegacyRuntimeProcessCommand(cleanTemplate, fallback, cleanStartFile, cleanDataDir)
	if reason := validateRuntimeProcessCommand(fallback); reason != "" {
		return "", "", inspected, fmt.Errorf("%s", reason)
	}
	if fallback != "" {
		return fallback, "template-default", inspected, nil
	}

	return "", "", inspected, fmt.Errorf("no startup command could be inferred")
}

func metaIntValue(meta map[string]any, key string, fallback int) int {
	if meta == nil {
		return fallback
	}
	resources, _ := meta["resources"].(map[string]any)
	if resources == nil {
		return fallback
	}
	switch v := resources[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return parsed
		}
	}
	return fallback
}

func metaFloatValue(meta map[string]any, key string, fallback float64) float64 {
	if meta == nil {
		return fallback
	}
	resources, _ := meta["resources"].(map[string]any)
	if resources == nil {
		return fallback
	}
	switch v := resources[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case string:
		if parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return parsed
		}
	}
	return fallback
}

func effectiveRamMb(resources *resourceLimits, meta map[string]any, fallback int) int {
	if resources != nil && resources.RamMb != nil && *resources.RamMb > 0 {
		return *resources.RamMb
	}
	return metaIntValue(meta, "ramMb", fallback)
}

func effectiveCpuCores(resources *resourceLimits, meta map[string]any, fallback float64) float64 {
	if resources != nil && resources.CpuCores != nil && *resources.CpuCores > 0 {
		return *resources.CpuCores
	}
	return metaFloatValue(meta, "cpuCores", fallback)
}

func resourceLimitsFromMeta(meta map[string]any) *resourceLimits {
	rm, ok := meta["resources"].(map[string]any)
	if !ok || rm == nil {
		return nil
	}
	resources := &resourceLimits{}
	if v := anyToInt(rm["ramMb"]); v > 0 {
		i := v
		resources.RamMb = &i
	}
	if v := anyToFloat64(rm["cpuCores"]); v > 0 {
		resources.CpuCores = &v
	}
	if v := anyToInt(rm["storageMb"]); v > 0 {
		i := v
		resources.StorageMb = &i
	} else if v := anyToInt(rm["storageGb"]); v > 0 {
		i := v * 1024
		resources.StorageMb = &i
	}
	if _, ok := rm["swapMb"]; ok {
		i := anyToInt(rm["swapMb"])
		resources.SwapMb = &i
	}
	if v := anyToInt(rm["backupsMax"]); v > 0 {
		i := v
		resources.BackupsMax = &i
	}
	if v := anyToInt(rm["maxSchedules"]); v > 0 {
		i := v
		resources.MaxSchedules = &i
	}
	if v := anyToInt(rm["ioWeight"]); v > 0 {
		i := v
		resources.IoWeight = &i
	}
	if v := anyToInt(rm["cpuWeight"]); v > 0 {
		i := v
		resources.CpuWeight = &i
	}
	if v := anyToInt(rm["pidsLimit"]); v > 0 {
		i := v
		resources.PidsLimit = &i
	}
	if v := anyToInt(rm["fileLimit"]); v > 0 {
		i := v
		resources.FileLimit = &i
	}
	if v := anyToInt64(rm["networkInboundLimitMb"]); v > 0 {
		i := v
		resources.NetworkInboundLimitMb = &i
	}
	if v := anyToInt64(rm["networkOutboundLimitMb"]); v > 0 {
		i := v
		resources.NetworkOutboundLimitMb = &i
	}
	if v := anyToInt(rm["networkResetMinutes"]); v > 0 {
		i := v
		resources.NetworkResetMinutes = &i
	}
	return resources
}

func renderRuntimeCommandTemplate(command, dataDir, startFile, imageRef string, hostPort int, resources *resourceLimits, meta map[string]any) string {
	cmd := sanitizeRuntimeCommand(command)
	if cmd == "" {
		return ""
	}
	ramMb := effectiveRamMb(resources, meta, 2048)
	cpuCores := effectiveCpuCores(resources, meta, 1)
	values := map[string]string{
		"PORT":          fmt.Sprint(hostPort),
		"SERVER_PORT":   fmt.Sprint(hostPort),
		"DATA_DIR":      dataDir,
		"START_FILE":    startFile,
		"RAM_MB":        fmt.Sprint(ramMb),
		"SERVER_MEMORY": fmt.Sprint(ramMb),
		"CPU_CORES":     strconv.FormatFloat(cpuCores, 'f', -1, 64),
		"IMAGE":         imageRef,
	}
	if meta != nil {
		if runtimeObj, ok := meta["runtime"].(map[string]any); ok {
			for key, value := range runtimeEnvMap(runtimeObj["env"]) {
				envKey := strings.TrimSpace(key)
				if envKey != "" {
					values[envKey] = value
				}
			}
		}
	}
	replacer := strings.NewReplacer(
		"{PORT}", values["PORT"],
		"{SERVER_PORT}", values["SERVER_PORT"],
		"{DATA_DIR}", values["DATA_DIR"],
		"{START_FILE}", values["START_FILE"],
		"{RAM_MB}", values["RAM_MB"],
		"{SERVER_MEMORY}", values["SERVER_MEMORY"],
		"{CPU_CORES}", values["CPU_CORES"],
		"{IMAGE}", values["IMAGE"],
	)
	rendered := replacer.Replace(cmd)
	doubleBraceReplacer := regexp.MustCompile(`\{\{\s*([A-Za-z0-9_.-]+)\s*\}\}`)
	rendered = doubleBraceReplacer.ReplaceAllStringFunc(rendered, func(token string) string {
		match := doubleBraceReplacer.FindStringSubmatch(token)
		if len(match) != 2 {
			return token
		}
		key := strings.TrimSpace(match[1])
		if key == "server.build.default.port" {
			if hostPort > 0 {
				return fmt.Sprint(hostPort)
			}
			return token
		}
		if strings.HasPrefix(key, "env.") {
			key = strings.TrimPrefix(key, "env.")
		}
		if value, ok := values[key]; ok {
			return value
		}
		return token
	})
	return strings.TrimSpace(rendered)
}

func isPythonRuntimeExecutable(execName string) bool {
	execName = strings.ToLower(strings.TrimSpace(execName))
	return execName == "python" || strings.HasPrefix(execName, "python3") || strings.HasPrefix(execName, "python2")
}

func isNodeRuntimeExecutable(execName string) bool {
	execName = strings.ToLower(strings.TrimSpace(execName))
	return execName == "node" || execName == "nodejs"
}

func firstRuntimeCommandFileTarget(args []string, startIndex int, extensions map[string]bool) string {
	for i := startIndex + 1; i < len(args); i++ {
		token := strings.TrimSpace(args[i])
		if token == "" || token == "--" {
			continue
		}
		if strings.HasPrefix(token, "-") {
			continue
		}
		ext := strings.ToLower(path.Ext(strings.SplitN(token, "?", 2)[0]))
		if extensions[ext] {
			return token
		}
	}
	return ""
}

func containerPathToServerPath(serverDir, dataDir, workdir, rawPath string) string {
	clean := strings.TrimSpace(rawPath)
	if clean == "" || strings.Contains(clean, "\x00") {
		return ""
	}
	clean = strings.ReplaceAll(clean, "\\", "/")

	var rel string
	if path.IsAbs(clean) {
		for _, root := range []string{dataDir, workdir} {
			root = path.Clean(strings.TrimSpace(root))
			if root == "" || root == "." || root == "/" {
				continue
			}
			if clean == root {
				return ""
			}
			prefix := strings.TrimRight(root, "/") + "/"
			if strings.HasPrefix(clean, prefix) {
				rel = strings.TrimPrefix(clean, prefix)
				break
			}
		}
		if rel == "" {
			return ""
		}
	} else {
		rel = clean
	}

	rel = path.Clean("/" + rel)
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return ""
	}
	return filepath.Join(serverDir, filepath.FromSlash(rel))
}

func ensureRuntimeEntrypointScaffold(serverDir, tpl, commandTemplate, dataDir, workdir, startFile string, hostPort int) {
	command := sanitizeRuntimeCommand(commandTemplate)
	if command == "" {
		return
	}
	args := parseShellArgs(command)
	execName, execIndex := resolveRuntimeProcessExecutable(args)
	if execIndex < 0 {
		return
	}

	if isPythonRuntimeExecutable(execName) {
		target := firstRuntimeCommandFileTarget(args, execIndex, map[string]bool{".py": true})
		if target == "" && strings.EqualFold(strings.TrimSpace(tpl), "python") {
			if startFile == "" {
				startFile = "main.py"
			}
			target = path.Join(strings.TrimRight(dataDir, "/"), startFile)
		}
		hostPath := containerPathToServerPath(serverDir, dataDir, workdir, target)
		if hostPath != "" {
			writeGeneratedRuntimeFile(hostPath, []byte(pythonMainTemplate), isLegacyPythonScaffold)
		}
		return
	}

	if isNodeRuntimeExecutable(execName) {
		target := firstRuntimeCommandFileTarget(args, execIndex, map[string]bool{".js": true, ".mjs": true, ".cjs": true})
		normalizedTpl := strings.ToLower(strings.TrimSpace(tpl))
		if target == "" && (normalizedTpl == "nodejs" || normalizedTpl == "discord-bot") {
			if startFile == "" {
				startFile = "index.js"
			}
			target = path.Join(strings.TrimRight(dataDir, "/"), startFile)
		}
		hostPath := containerPathToServerPath(serverDir, dataDir, workdir, target)
		if hostPath != "" {
			writeGeneratedRuntimeFile(hostPath, []byte(buildNodeIndexTemplate(hostPort)), isLegacyNodeScaffold)
		}
	}
}

func parseVolumeSpecContainerPath(spec string) string {
	parts := strings.Split(spec, ":")
	if len(parts) < 2 {
		return ""
	}
	containerPath := strings.TrimSpace(parts[1])
	if idx := strings.LastIndex(containerPath, ":"); idx > 0 {
		containerPath = containerPath[:idx]
	}
	return strings.TrimSpace(containerPath)
}

func replaceVolumeSpecContainerPath(spec, containerPath string) string {
	cleanTarget := strings.TrimSpace(containerPath)
	if cleanTarget == "" {
		return spec
	}
	parts := strings.Split(spec, ":")
	if len(parts) < 2 {
		return spec
	}
	parts[1] = cleanTarget
	return strings.Join(parts, ":")
}

func imageConfigHasRuntimeDefault(cfg imageConfig) bool {
	return len(trimRuntimeProcessArgs(cfg.Entrypoint)) > 0 || len(trimRuntimeProcessArgs(cfg.Cmd)) > 0
}

func imageConfigDeclaresVolume(cfg imageConfig, containerPath string) bool {
	cleanPath := path.Clean(strings.TrimSpace(containerPath))
	if cleanPath == "." || cleanPath == "/" || cleanPath == "" {
		return false
	}
	for _, volumePath := range cfg.Volumes {
		if path.Clean(strings.TrimSpace(volumePath)) == cleanPath {
			return true
		}
	}
	return false
}

func imageUsesPterodactylContainerWorkdir(imageRef string, imgCfg imageConfig) bool {
	workdir := path.Clean(strings.TrimSpace(imgCfg.Workdir))
	if workdir != "/home/container" {
		return false
	}
	imageLower := strings.ToLower(strings.TrimSpace(imageRef))
	return strings.Contains(imageLower, "ghcr.io/pterodactyl/") ||
		strings.Contains(imageLower, "ghcr.io/ptero-eggs/") ||
		strings.Contains(imageLower, "ghcr.io/parkervcp/") ||
		strings.Contains(imageLower, "pterodactyl/")
}

func (a *Agent) inspectRuntimeImageConfig(ctx context.Context, imageRef string) imageConfig {
	cleanImageRef := strings.TrimSpace(imageRef)
	if cleanImageRef == "" {
		return imageConfig{Env: map[string]string{}}
	}
	if !a.docker.imageExistsLocally(ctx, cleanImageRef) {
		a.docker.pull(ctx, cleanImageRef)
	}
	return a.docker.inspectImageConfig(ctx, cleanImageRef)
}

func sanitizeCustomImageDefaultBinds(imageRef string, imgCfg imageConfig, binds []string, dataDir string) ([]string, string) {
	workdir := path.Clean(strings.TrimSpace(imgCfg.Workdir))
	if workdir == "" || workdir == "." || workdir == "/" || !imageConfigHasRuntimeDefault(imgCfg) || imageConfigDeclaresVolume(imgCfg, workdir) {
		return binds, dataDir
	}
	if imageUsesPterodactylContainerWorkdir(imageRef, imgCfg) {
		return binds, dataDir
	}

	gameDataPath := ""
	if isGameServerDockerImage(imageRef) {
		gameDataPath = path.Clean(getGameServerDataPath(imageRef))
	}
	if gameDataPath == workdir {
		return binds, dataDir
	}
	if gameDataPath == "." || gameDataPath == "/" {
		gameDataPath = ""
	}

	changed := false
	out := make([]string, 0, len(binds))
	for _, bind := range binds {
		containerPath := path.Clean(parseVolumeSpecContainerPath(bind))
		if containerPath == workdir {
			if gameDataPath != "" {
				out = append(out, replaceVolumeSpecContainerPath(bind, gameDataPath))
				if path.Clean(strings.TrimSpace(dataDir)) == workdir {
					dataDir = gameDataPath
				}
				changed = true
				fmt.Printf("[docker] remapped volume away from image workdir for %s: %s -> %s\n", imageRef, workdir, gameDataPath)
				continue
			}
			changed = true
			fmt.Printf("[docker] skipped volume that would hide image workdir for %s: %s\n", imageRef, bind)
			continue
		}
		out = append(out, bind)
	}

	if !changed {
		return binds, dataDir
	}
	if len(out) == 0 {
		return nil, ""
	}
	return out, dataDir
}

func runtimeStringSlice(raw any) []string {
	switch v := raw.(type) {
	case []string:
		out := make([]string, 0, len(v))
		for _, entry := range v {
			clean := strings.TrimSpace(entry)
			if clean != "" && clean != "<nil>" {
				out = append(out, clean)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(v))
		for _, entry := range v {
			clean := strings.TrimSpace(fmt.Sprint(entry))
			if clean != "" && clean != "<nil>" {
				out = append(out, clean)
			}
		}
		return out
	default:
		return nil
	}
}

func runtimeIntSlice(raw any) []int {
	switch v := raw.(type) {
	case []int:
		out := make([]int, 0, len(v))
		for _, entry := range v {
			if port, ok := parseStrictPortValue(entry); ok {
				out = append(out, port)
			}
		}
		return out
	case []float64:
		out := make([]int, 0, len(v))
		for _, entry := range v {
			if port, ok := parseStrictPortValue(entry); ok {
				out = append(out, port)
			}
		}
		return out
	case []string:
		out := make([]int, 0, len(v))
		for _, entry := range v {
			if port, ok := parseStrictPortValue(entry); ok {
				out = append(out, port)
			}
		}
		return out
	case []any:
		out := make([]int, 0, len(v))
		for _, entry := range v {
			if port, ok := parseStrictPortValue(entry); ok {
				out = append(out, port)
			}
		}
		return out
	default:
		return nil
	}
}

func runtimeBool(raw any) bool {
	switch v := raw.(type) {
	case bool:
		return v
	case string:
		clean := strings.ToLower(strings.TrimSpace(v))
		return clean == "1" || clean == "true" || clean == "yes" || clean == "on"
	case float64:
		return v != 0
	case int:
		return v != 0
	default:
		return false
	}
}

func runtimeEnvMap(raw any) map[string]string {
	out := map[string]string{}
	switch v := raw.(type) {
	case map[string]string:
		for key, value := range v {
			envKey := strings.TrimSpace(key)
			if envKey != "" {
				out[envKey] = value
			}
		}
	case map[string]any:
		for key, value := range v {
			envKey := strings.TrimSpace(key)
			if envKey != "" {
				out[envKey] = fmt.Sprint(value)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func primaryPortEnvKeysForTemplate(tpl string) []string {
	switch strings.ToLower(strings.TrimSpace(tpl)) {
	case "ark-survival-evolved", "ark":
		return []string{"GAME_CLIENT_PORT"}
	case "cs2", "dayz", "rust", "valheim":
		return []string{"SERVER_PORT"}
	case "fivem", "redm":
		return []string{"SERVER_PORT", "PORT", "ADPANEL_GAME_PORT"}
	case "palworld":
		return []string{"PORT"}
	case "project-zomboid":
		return []string{"DEFAULT_PORT"}
	case "ragemp":
		return []string{"RAGEMP_PORT"}
	default:
		return nil
	}
}

func applyRuntimeHostPortEnv(tpl string, env map[string]string, hostPort int) {
	if env == nil || hostPort <= 0 {
		return
	}
	portValue := fmt.Sprint(hostPort)
	env["PORT"] = portValue
	env["SERVER_PORT"] = portValue
	for _, key := range primaryPortEnvKeysForTemplate(tpl) {
		env[key] = portValue
	}
}

func renderRuntimeEnvTemplates(tpl string, env map[string]string, hostPort int) {
	if env == nil {
		return
	}
	for key, value := range env {
		if !strings.Contains(value, "{{") && !strings.Contains(value, "{PORT}") && !strings.Contains(value, "{SERVER_PORT}") {
			continue
		}
		rendered := renderADPanelStartupTemplate(value, env, hostPort)
		if hostPort > 0 {
			rendered = strings.ReplaceAll(rendered, "{SERVER_PORT}", fmt.Sprint(hostPort))
			rendered = strings.ReplaceAll(rendered, "{PORT}", fmt.Sprint(hostPort))
		}
		env[key] = rendered
	}
}

func runtimeUsesHostPortInsideContainer(tpl string, runtimeObj map[string]any, env map[string]string) bool {
	normalized := strings.ToLower(strings.TrimSpace(tpl))
	switch normalized {
	case "ark-survival-evolved", "ark", "cs2", "dayz", "fivem", "redm", "palworld", "project-zomboid", "ragemp", "rust", "valheim":
		return true
	}
	for _, key := range primaryPortEnvKeysForTemplate(normalized) {
		if _, ok := env[key]; ok {
			return true
		}
	}
	startup := strings.TrimSpace(env["STARTUP"])
	if startup == "" {
		startup = startupTemplateFromRuntime(runtimeObj)
	}
	return strings.Contains(startup, "{{SERVER_PORT}}") ||
		strings.Contains(startup, "{{PORT}}") ||
		strings.Contains(startup, "{SERVER_PORT}") ||
		strings.Contains(startup, "{PORT}")
}

const cfxRuntimeServerDir = "/serverdata/serverfiles"

func isCfxTemplate(tpl string) bool {
	switch strings.ToLower(strings.TrimSpace(tpl)) {
	case "fivem", "five-m", "redm":
		return true
	default:
		return false
	}
}

func migrateNestedCfxServerFiles(serverDir string) {
	nested := filepath.Join(serverDir, "serverfiles")
	info, err := os.Stat(nested)
	if err != nil || !info.IsDir() {
		return
	}
	entries, err := os.ReadDir(nested)
	if err != nil {
		return
	}
	for _, entry := range entries {
		src := filepath.Join(nested, entry.Name())
		dst := filepath.Join(serverDir, entry.Name())
		if pathExists(dst) {
			continue
		}
		if err := os.Rename(src, dst); err != nil {
			fmt.Printf("[cfx-layout] could not move %s to %s: %v\n", src, dst, err)
		}
	}
	_ = os.Remove(nested)
}

func normalizeCfxRuntimeLayout(tpl string, runtimeObj map[string]any, serverDir string) bool {
	if runtimeObj == nil || !isCfxTemplate(tpl) {
		return false
	}
	changed := false
	migrateNestedCfxServerFiles(serverDir)
	desiredVolume := "{BOT_DIR}:" + cfxRuntimeServerDir
	volumes := runtimeStringSlice(runtimeObj["volumes"])
	if len(volumes) != 1 || strings.TrimSpace(volumes[0]) != desiredVolume {
		runtimeObj["volumes"] = []any{desiredVolume}
		changed = true
	}
	if normalizeContainerWorkdir(runtimeObj["workdir"]) != cfxRuntimeServerDir {
		runtimeObj["workdir"] = cfxRuntimeServerDir
		changed = true
	}
	env := runtimeEnvMap(runtimeObj["env"])
	if env == nil {
		env = map[string]string{}
	}
	for key, value := range map[string]string{
		"DATA_DIR":    cfxRuntimeServerDir,
		"SERVER_DIR":  cfxRuntimeServerDir,
		"GAME_CONFIG": "server.cfg",
		"HOME":        cfxRuntimeServerDir,
		"DATA_PERM":   "777",
	} {
		if env[key] != value {
			env[key] = value
			changed = true
		}
	}
	runtimeObj["env"] = env
	if runtimeObj["runAsRoot"] != true {
		runtimeObj["runAsRoot"] = true
		changed = true
	}
	if rawInstall, ok := runtimeObj["install"]; ok && rawInstall != nil {
		switch install := rawInstall.(type) {
		case map[string]any:
			if normalizeContainerWorkdir(install["runtimePath"]) != cfxRuntimeServerDir {
				install["runtimePath"] = cfxRuntimeServerDir
				changed = true
			}
			if normalizeContainerWorkdir(install["mountPath"]) == "" && normalizeContainerWorkdir(install["containerPath"]) == "" {
				install["mountPath"] = "/mnt/server"
				changed = true
			}
		case *templateInstallConfig:
			if normalizeContainerWorkdir(install.RuntimePath) != cfxRuntimeServerDir {
				install.RuntimePath = cfxRuntimeServerDir
				changed = true
			}
			if normalizeContainerWorkdir(install.MountPath) == "" && normalizeContainerWorkdir(install.ContainerPath) == "" {
				install.MountPath = "/mnt/server"
				changed = true
			}
		}
	}
	return changed
}

func (a *Agent) resolveStructuredBinds(tpl string, runtimeObj map[string]any, serverDir string) ([]string, string) {
	defaultDir := defaultDataDirForType(tpl)
	binds := []string{}
	primaryDir := defaultDir

	if volumes := runtimeStringSlice(runtimeObj["volumes"]); len(volumes) > 0 {
		for _, s := range volumes {
			s = strings.ReplaceAll(s, "{BOT_DIR}", serverDir)
			s = strings.ReplaceAll(s, "{SERVER_DIR}", serverDir)
			s = strings.ReplaceAll(s, "{DATA_DIR}", serverDir)

			parts := strings.SplitN(s, ":", 2)
			if len(parts) >= 2 {
				hostPath := filepath.Clean(parts[0])
				if err := ensureResolvedUnder(a.volumesDir, hostPath); err != nil {
					fmt.Printf("[security] blocked volume mount outside volumes dir: %s\n", s)
					continue
				}
				containerPath := parseVolumeSpecContainerPath(s)
				if containerPath != "" && primaryDir == defaultDir {
					primaryDir = containerPath
				}
			}
			binds = append(binds, s)
		}
	}

	if len(binds) == 0 {
		binds = []string{fmt.Sprintf("%s:%s", serverDir, defaultDir)}
		primaryDir = defaultDir
	}
	return binds, primaryDir
}

func addPortBinding(portBindings map[string][]dockerPortBinding, exposed map[string]struct{}, hostPort, containerPort int, protocols []string) {
	if hostPort <= 0 || containerPort <= 0 {
		return
	}
	cleanProtocols := normalizePortProtocolSlice(protocols)
	if len(cleanProtocols) == 0 {
		cleanProtocols = []string{"tcp"}
	}
	for _, protocol := range cleanProtocols {
		key := fmt.Sprintf("%d/%s", containerPort, protocol)
		exposed[key] = struct{}{}
		duplicate := false
		for _, binding := range portBindings[key] {
			if binding.HostPort == fmt.Sprint(hostPort) {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		portBindings[key] = append(portBindings[key], dockerPortBinding{
			HostIP:   "0.0.0.0",
			HostPort: fmt.Sprint(hostPort),
		})
	}
}

func (a *Agent) buildStructuredPortBindings(serverDir string, meta map[string]any, hostPort, mainContainerPort int) (map[string][]dockerPortBinding, map[string]struct{}) {
	portBindings := make(map[string][]dockerPortBinding)
	exposed := make(map[string]struct{})
	runtimeObj, _ := meta["runtime"].(map[string]any)
	protocolsByPort := runtimePortProtocolMap(runtimeObj["portProtocols"])
	addPortBinding(portBindings, exposed, hostPort, mainContainerPort, portProtocolsFor(protocolsByPort, mainContainerPort))

	if metaRes, ok := meta["resources"].(map[string]any); ok {
		for _, ap := range runtimeIntSlice(metaRes["ports"]) {
			if ap > 0 && ap != hostPort {
				addPortBinding(portBindings, exposed, ap, ap, portProtocolsFor(protocolsByPort, ap))
				fmt.Printf("[port-manage] exposing additional port %d:%d\n", ap, ap)
			}
		}
	}

	for _, pf := range a.loadPortForwards(serverDir) {
		targetPort := pf.Internal
		if hostPort > 0 && pf.Internal == hostPort && mainContainerPort > 0 {
			targetPort = mainContainerPort
		}
		addPortBinding(portBindings, exposed, pf.Public, targetPort, portProtocolsFor(protocolsByPort, targetPort))
	}
	return portBindings, exposed
}

func dockerRestartPolicyFromName(name string) dockerRestartPolicy {
	clean := strings.ToLower(strings.TrimSpace(name))
	switch clean {
	case "no", "":
		return dockerRestartPolicy{Name: "no"}
	case "always":
		return dockerRestartPolicy{Name: "always"}
	case "on-failure":
		return dockerRestartPolicy{Name: "on-failure"}
	default:
		return dockerRestartPolicy{Name: "unless-stopped"}
	}
}

func buildStructuredHostConfig(binds []string, portBindings map[string][]dockerPortBinding, resources *resourceLimits, restart string, readOnly bool) dockerHostConfig {
	pidsLimit := int64(defaultPidsLimit)
	if resources != nil && resources.PidsLimit != nil && *resources.PidsLimit >= minPidsLimit && *resources.PidsLimit <= maxPidsLimit {
		pidsLimit = int64(*resources.PidsLimit)
	}

	hostConfig := dockerHostConfig{
		Binds:         binds,
		PortBindings:  portBindings,
		RestartPolicy: dockerRestartPolicyFromName(restart),
		CapDrop:       []string{"ALL"},
		CapAdd: []string{
			"CHOWN",
			"DAC_OVERRIDE",
			"FOWNER",
			"SETUID",
			"SETGID",
			"KILL",
			"NET_BIND_SERVICE",
			"NET_RAW",
			"SYS_CHROOT",
			"AUDIT_WRITE",
		},
		SecurityOpt: []string{"no-new-privileges"},
		PidsLimit:   pidsLimit,
	}

	if readOnly {
		hostConfig.ReadonlyRootfs = true
		hostConfig.Tmpfs = map[string]string{
			"/tmp":     "rw,noexec,nosuid,size=256m",
			"/var/tmp": "rw,noexec,nosuid,size=64m",
			"/run":     "rw,noexec,nosuid,size=32m",
		}
	}

	if resources != nil {
		if resources.RamMb != nil && *resources.RamMb > 0 {
			hostConfig.Memory = int64(*resources.RamMb) * 1024 * 1024
			hostConfig.MemoryReservation = int64(*resources.RamMb*90/100) * 1024 * 1024
			if resources.SwapMb == nil {
				hostConfig.MemorySwap = int64(*resources.RamMb) * 1024 * 1024
			} else {
				swapVal := *resources.SwapMb
				if swapVal == -1 {
					hostConfig.MemorySwap = int64(*resources.RamMb) * 1024 * 1024
				} else if swapVal == 0 {
					hostConfig.MemorySwap = -1
				} else {
					hostConfig.MemorySwap = int64(*resources.RamMb+swapVal) * 1024 * 1024
				}
			}
		}

		if resources.CpuCores != nil && *resources.CpuCores > 0 {
			effCores, _ := computeCpuLimit(*resources.CpuCores)
			hostConfig.CpuPeriod = cpuPeriod
			hostConfig.CpuQuota = int64(float64(cpuPeriod) * effCores)
		}
		if resources.CpuWeight != nil && *resources.CpuWeight >= minCpuWeight && *resources.CpuWeight <= maxCpuWeight {
			hostConfig.CpuShares = dockerCpuSharesFromWeight(*resources.CpuWeight)
		}
		if resources.IoWeight != nil && *resources.IoWeight >= minIoWeight && *resources.IoWeight <= maxIoWeight {
			hostConfig.BlkioWeight = int64(*resources.IoWeight)
		}
		if resources.FileLimit != nil && *resources.FileLimit >= minFileLimit && *resources.FileLimit <= maxFileLimit {
			limit := int64(*resources.FileLimit)
			hostConfig.Ulimits = append(hostConfig.Ulimits, dockerUlimit{Name: "nofile", Soft: limit, Hard: limit})
		}
	}

	return hostConfig
}

func envMapToList(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		out = append(out, fmt.Sprintf("%s=%s", k, v))
	}
	return out
}

func isBlockedRuntimeShellExecutable(arg string) bool {
	switch runtimeProcessBaseExecutable(arg) {
	case "sh", "bash", "ash", "dash", "zsh", "ksh":
		return true
	default:
		return false
	}
}

func isRuntimeShellWrapperArgs(args []string, execIndex int) bool {
	if execIndex < 0 || execIndex >= len(args) {
		return false
	}
	if !isBlockedRuntimeShellExecutable(args[execIndex]) {
		return false
	}
	if execIndex+1 >= len(args) {
		return false
	}
	flags := strings.ToLower(strings.TrimSpace(args[execIndex+1]))
	return strings.HasPrefix(flags, "-") && strings.Contains(flags, "c")
}

func isBlockedRuntimeShellToken(arg string) bool {
	switch strings.TrimSpace(arg) {
	case "&&", "||", "|", ";", "&", ">", ">>", "<", "<<", "2>", "2>>", "2>&1", "|&":
		return true
	default:
		return false
	}
}

func runtimeProcessBaseExecutable(arg string) string {
	clean := strings.TrimSpace(arg)
	if clean == "" {
		return ""
	}
	if strings.IndexFunc(clean, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }) >= 0 {
		if nested := parseShellArgs(clean); len(nested) > 0 {
			clean = strings.TrimSpace(nested[0])
		}
	}
	clean = strings.ToLower(strings.ReplaceAll(clean, "\\", "/"))
	if slash := strings.LastIndex(clean, "/"); slash >= 0 && slash+1 < len(clean) {
		return clean[slash+1:]
	}
	return clean
}

func isRuntimeEnvAssignmentToken(arg string) bool {
	token := strings.TrimSpace(arg)
	if token == "" || strings.HasPrefix(token, "-") {
		return false
	}
	eqIdx := strings.Index(token, "=")
	if eqIdx <= 0 {
		return false
	}
	key := token[:eqIdx]
	for i, r := range key {
		isLetter := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		isDigit := r >= '0' && r <= '9'
		if i == 0 {
			if r != '_' && !isLetter {
				return false
			}
			continue
		}
		if r != '_' && !isLetter && !isDigit {
			return false
		}
	}
	return true
}

func resolveRuntimeProcessExecutable(args []string) (string, int) {
	if len(args) == 0 {
		return "", -1
	}
	for index := 0; index < len(args); index++ {
		token := strings.TrimSpace(args[index])
		if token == "" {
			continue
		}
		execName := runtimeProcessBaseExecutable(token)
		if execName == "env" {
			index++
			for index < len(args) {
				envToken := strings.TrimSpace(args[index])
				if envToken == "" {
					index++
					continue
				}
				if envToken == "--" {
					index++
					break
				}
				if strings.HasPrefix(envToken, "-") || isRuntimeEnvAssignmentToken(envToken) {
					index++
					continue
				}
				break
			}
			index--
			continue
		}
		return execName, index
	}
	return "", -1
}

func isBlockedContainerRuntimeExecutable(arg string) bool {
	switch strings.TrimSpace(arg) {
	case "docker", "docker-compose", "podman", "podman-compose", "nerdctl", "ctr":
		return true
	default:
		return false
	}
}

func unwrapRuntimeShellWrapper(cmd string) (string, bool) {
	args := parseShellArgs(cmd)
	if len(args) < 3 {
		return "", false
	}
	if !isBlockedRuntimeShellExecutable(args[0]) {
		return "", false
	}
	flags := strings.TrimSpace(args[1])
	if !strings.HasPrefix(flags, "-") || !strings.Contains(flags, "c") {
		return "", false
	}
	inner := sanitizeRuntimeCommand(args[2])
	if inner == "" {
		return "", false
	}
	return inner, true
}

func normalizeLegacyRuntimeProcessCommand(tpl, command, startFile, dataDir string) string {
	clean := sanitizeRuntimeCommand(command)
	if clean == "" {
		return ""
	}
	for i := 0; i < 2; i++ {
		inner, ok := unwrapRuntimeShellWrapper(clean)
		if !ok {
			break
		}
		clean = sanitizeRuntimeCommand(inner)
	}
	switch strings.ToLower(strings.TrimSpace(tpl)) {
	case "nodejs", "discord-bot":
		lower := strings.ToLower(clean)
		nodeIdx := strings.LastIndex(lower, "node ")
		if nodeIdx >= 0 && (strings.Contains(lower, "&&") || strings.HasPrefix(lower, "npm ") || strings.HasPrefix(lower, "cd ")) {
			clean = sanitizeRuntimeCommand(clean[nodeIdx:])
		}
	}
	return clean
}

func looksLikePathOnlyScriptRuntimeCommand(command string) bool {
	args := parseShellArgs(command)
	if len(args) != 1 {
		return false
	}
	token := strings.ToLower(strings.TrimSpace(args[0]))
	if !strings.HasPrefix(token, "/") {
		return false
	}
	return strings.Contains(token, "entrypoint") || strings.HasSuffix(token, ".sh") || strings.HasSuffix(token, ".bash")
}

func looksLikeImageDefaultWrapperRuntimeCommand(command string, imgCfg imageConfig) bool {
	clean := strings.ToLower(strings.TrimSpace(command))
	if clean == "" {
		return false
	}
	if looksLikePathOnlyScriptRuntimeCommand(command) {
		return true
	}
	entrypoint := trimRuntimeProcessArgs(imgCfg.Entrypoint)
	cmd := trimRuntimeProcessArgs(imgCfg.Cmd)
	if len(entrypoint) == 2 && isRuntimeShellName(entrypoint[0]) {
		script := strings.ToLower(strings.TrimSpace(entrypoint[1]))
		if strings.HasPrefix(script, "/") && (strings.Contains(script, "entrypoint") || strings.HasSuffix(script, ".sh") || strings.HasSuffix(script, ".bash")) {
			return true
		}
	}
	if len(cmd) == 1 {
		script := strings.ToLower(strings.TrimSpace(cmd[0]))
		if strings.HasPrefix(script, "/") && (strings.Contains(script, "entrypoint") || strings.HasSuffix(script, ".sh") || strings.HasSuffix(script, ".bash")) {
			return true
		}
	}
	if strings.Contains(clean, "entrypoint") && strings.Contains(clean, "/") {
		return true
	}
	return false
}

func shouldPreferImageDefaultRuntimeCommand(command string, imgCfg imageConfig) bool {
	if !imageConfigHasRuntimeDefault(imgCfg) {
		return false
	}
	return looksLikeImageDefaultWrapperRuntimeCommand(command, imgCfg)
}

func validateRuntimeProcessCommand(command string) string {
	clean := sanitizeRuntimeCommand(command)
	if clean == "" {
		return ""
	}
	if len(clean) > 4000 {
		return "process command is too long"
	}
	if strings.Contains(clean, "\n") {
		return "process command must be a single line"
	}
	if strings.Contains(clean, "`") || strings.Contains(clean, "$(") {
		return "shell expansions are not allowed in process commands"
	}
	args := parseShellArgs(clean)
	if len(args) == 0 {
		return "process command is empty"
	}
	execName, execIndex := resolveRuntimeProcessExecutable(args)
	if isBlockedContainerRuntimeExecutable(execName) {
		return `container runtime CLI commands like "docker run" are not allowed; configure image, ports, volumes, env, and the in-container process separately`
	}
	if isBlockedRuntimeShellExecutable(execName) && isRuntimeShellWrapperArgs(args, execIndex) {
		return "shell wrappers like sh -c or bash -lc are not allowed; provide the executable and arguments directly"
	}
	for _, arg := range args {
		if isBlockedRuntimeShellToken(arg) {
			return fmt.Sprintf("shell control operator %q is not allowed in process commands", strings.TrimSpace(arg))
		}
	}
	return ""
}

func buildProcessCommandArgs(command string) ([]string, error) {
	clean := sanitizeRuntimeCommand(command)
	if clean == "" {
		return nil, nil
	}
	if reason := validateRuntimeProcessCommand(clean); reason != "" {
		return nil, fmt.Errorf("%s", reason)
	}
	args := parseShellArgs(clean)
	if len(args) == 0 {
		return nil, fmt.Errorf("process command is empty")
	}
	return args, nil
}

func normalizeContainerWorkdir(value any) string {
	workdir := strings.TrimSpace(fmt.Sprint(value))
	if workdir == "" || workdir == "<nil>" {
		return ""
	}
	workdir = strings.ReplaceAll(workdir, "\\", "/")
	if !strings.HasPrefix(workdir, "/") {
		return ""
	}
	workdir = path.Clean(workdir)
	if workdir == "" || workdir == "." {
		return ""
	}
	if !strings.HasPrefix(workdir, "/") {
		workdir = "/" + workdir
	}
	return workdir
}

func buildRenderedRuntimeProcessArgs(tpl, command, dataDir, startFile, imageRef string, hostPort int, resources *resourceLimits, meta map[string]any) ([]string, error) {
	rendered := renderRuntimeCommandTemplate(command, dataDir, startFile, imageRef, hostPort, resources, meta)
	rendered = normalizeLegacyRuntimeProcessCommand(tpl, rendered, startFile, dataDir)
	return buildProcessCommandArgs(rendered)
}

func (a *Agent) recentContainerLogs(ctx context.Context, name string, tail int) string {
	tailN := tail
	if tailN <= 0 {
		tailN = 50
	}
	out, _, _, _ := a.docker.runCollect(ctx, "logs", "--tail", fmt.Sprint(tailN), name)
	return out
}

func isImmediateStartupPermissionIssue(logs string) bool {
	lower := strings.ToLower(strings.TrimSpace(logs))
	if lower == "" {
		return false
	}

	hasPermissionFailure := strings.Contains(lower, "permission denied") ||
		strings.Contains(lower, "operation not permitted") ||
		strings.Contains(lower, "cannot execute")
	if !hasPermissionFailure {
		return false
	}

	startupHints := []string{
		"start.sh",
		"entrypoint",
		"docker-entrypoint",
		"/opt/",
		"/bin/bash",
		"/bin/sh",
		"/usr/local/bin",
		"exec /",
	}
	for _, hint := range startupHints {
		if strings.Contains(lower, hint) {
			return true
		}
	}

	return true
}

func isMissingRuntimeExecutableStartError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	if !strings.Contains(lower, "exec:") {
		return false
	}
	return strings.Contains(lower, "no such file or directory") ||
		strings.Contains(lower, "executable file not found") ||
		strings.Contains(lower, "stat ") ||
		strings.Contains(lower, "not found")
}

func (a *Agent) preferredStructuredContainerUser(ctx context.Context, imageRef string, uid, gid int) string {
	if uid < 0 || gid < 0 {
		return ""
	}

	cleanImageRef := strings.TrimSpace(imageRef)
	if cleanImageRef != "" {
		if !a.docker.imageExistsLocally(ctx, cleanImageRef) {
			a.docker.pull(ctx, cleanImageRef)
		}
		imgCfg := a.docker.inspectImageConfig(ctx, cleanImageRef)
		if strings.TrimSpace(imgCfg.User) != "" {
			if a.debug {
				fmt.Printf("[docker] honoring image-defined user %q for %s\n", imgCfg.User, cleanImageRef)
			}
			return ""
		}
	}

	return fmt.Sprintf("%d:%d", uid, gid)
}

func runtimeRequestsRootUser(runtimeObj map[string]any) bool {
	if runtimeObj == nil {
		return false
	}
	return runtimeBool(runtimeObj["runAsRoot"]) || runtimeBool(runtimeObj["run_as_root"])
}

func (a *Agent) structuredContainerUser(ctx context.Context, imageRef string, uid, gid int, runtimeObj map[string]any) string {
	if runtimeRequestsRootUser(runtimeObj) {
		if a.debug {
			fmt.Printf("[docker] template requested root entrypoint for %s\n", imageRef)
		}
		return ""
	}
	return a.preferredStructuredContainerUser(ctx, imageRef, uid, gid)
}

func hostConfigMemoryLimitMb(cfg dockerHostConfig) float64 {
	if cfg.Memory <= 0 {
		return 0
	}
	return float64(cfg.Memory) / (1024 * 1024)
}

func hostConfigCpuLimitPercent(cfg dockerHostConfig) float64 {
	if cfg.CpuPeriod <= 0 || cfg.CpuQuota <= 0 {
		return 0
	}
	return (float64(cfg.CpuQuota) / float64(cfg.CpuPeriod)) * 100
}

func almostEqualFloat64(a, b, tolerance float64) bool {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff <= tolerance
}

func (a *Agent) inspectContainerHostConfig(ctx context.Context, name string) (dockerHostConfig, error) {
	inspected, err := a.docker.inspectContainerAPI(ctx, dockerContainerName(name))
	if err != nil {
		return dockerHostConfig{}, err
	}
	return inspected.HostConfig, nil
}

func (a *Agent) inspectContainerAppliedLimits(ctx context.Context, name string) (memoryLimitMb float64, cpuLimitPercent float64, err error) {
	hostConfig, err := a.inspectContainerHostConfig(ctx, name)
	if err != nil {
		return 0, 0, err
	}
	return hostConfigMemoryLimitMb(hostConfig), hostConfigCpuLimitPercent(hostConfig), nil
}

type dockerContainerStateForStats struct {
	Status    string `json:"Status"`
	Running   bool   `json:"Running"`
	StartedAt string `json:"StartedAt"`
}

type dockerContainerInspectForStats struct {
	ID         string                       `json:"Id"`
	Name       string                       `json:"Name"`
	State      dockerContainerStateForStats `json:"State"`
	HostConfig dockerHostConfig             `json:"HostConfig"`
}

type dockerStatsCPUUsage struct {
	TotalUsage  uint64   `json:"total_usage"`
	PercpuUsage []uint64 `json:"percpu_usage"`
}

type dockerStatsCPU struct {
	CPUUsage       dockerStatsCPUUsage `json:"cpu_usage"`
	SystemCPUUsage uint64              `json:"system_cpu_usage"`
	OnlineCPUs     uint32              `json:"online_cpus"`
}

type dockerStatsMemory struct {
	Usage uint64            `json:"usage"`
	Limit uint64            `json:"limit"`
	Stats map[string]uint64 `json:"stats"`
}

type dockerStatsNetwork struct {
	RxBytes uint64 `json:"rx_bytes"`
	TxBytes uint64 `json:"tx_bytes"`
}

type dockerContainerStatsResponse struct {
	CPUStats    dockerStatsCPU                `json:"cpu_stats"`
	PreCPUStats dockerStatsCPU                `json:"precpu_stats"`
	MemoryStats dockerStatsMemory             `json:"memory_stats"`
	Networks    map[string]dockerStatsNetwork `json:"networks"`
}

func (a *Agent) inspectContainerForStats(ctx context.Context, name string) (dockerContainerInspectForStats, bool, error) {
	dockerName := dockerContainerName(name)
	body, status, err := a.docker.engineRequest(ctx, http.MethodGet, "/containers/"+url.PathEscape(dockerName)+"/json", nil, nil)
	if err != nil {
		return dockerContainerInspectForStats{}, false, err
	}
	if status == http.StatusNotFound {
		return dockerContainerInspectForStats{}, false, nil
	}
	if status < 200 || status >= 300 {
		return dockerContainerInspectForStats{}, false, fmt.Errorf("failed to inspect container %s: %s", name, decodeDockerAPIError(body))
	}

	var payload dockerContainerInspectForStats
	if err := json.Unmarshal(body, &payload); err != nil {
		return dockerContainerInspectForStats{}, false, err
	}
	if strings.TrimPrefix(payload.Name, "/") != dockerName {
		return dockerContainerInspectForStats{}, false, fmt.Errorf("container name mismatch")
	}
	return payload, true, nil
}

func (a *Agent) readContainerStatsForServer(ctx context.Context, name string) (dockerContainerStatsResponse, error) {
	query := url.Values{}
	query.Set("stream", "false")
	dockerName := dockerContainerName(name)
	body, status, err := a.docker.engineRequest(ctx, http.MethodGet, "/containers/"+url.PathEscape(dockerName)+"/stats", query, nil)
	if err != nil {
		return dockerContainerStatsResponse{}, err
	}
	if status < 200 || status >= 300 {
		return dockerContainerStatsResponse{}, fmt.Errorf("failed to read container stats %s: %s", name, decodeDockerAPIError(body))
	}
	var payload dockerContainerStatsResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return dockerContainerStatsResponse{}, err
	}
	return payload, nil
}

func dockerCPUPercent(stats dockerContainerStatsResponse) float64 {
	cpuTotal := stats.CPUStats.CPUUsage.TotalUsage
	preCPUTime := stats.PreCPUStats.CPUUsage.TotalUsage
	systemTotal := stats.CPUStats.SystemCPUUsage
	preSystemTotal := stats.PreCPUStats.SystemCPUUsage
	if cpuTotal <= preCPUTime || systemTotal <= preSystemTotal {
		return 0
	}

	cpuDelta := float64(cpuTotal - preCPUTime)
	systemDelta := float64(systemTotal - preSystemTotal)
	if cpuDelta <= 0 || systemDelta <= 0 {
		return 0
	}

	onlineCPUs := float64(stats.CPUStats.OnlineCPUs)
	if onlineCPUs <= 0 {
		onlineCPUs = float64(len(stats.CPUStats.CPUUsage.PercpuUsage))
	}
	if onlineCPUs <= 0 {
		onlineCPUs = 1
	}
	return (cpuDelta / systemDelta) * onlineCPUs * 100
}

func dockerMemoryUsedBytes(mem dockerStatsMemory) uint64 {
	usage := mem.Usage
	if usage == 0 {
		return 0
	}
	if inactiveFile, ok := mem.Stats["inactive_file"]; ok && inactiveFile < usage {
		return usage - inactiveFile
	}
	if cache, ok := mem.Stats["cache"]; ok && cache < usage {
		return usage - cache
	}
	return usage
}

func sumDockerNetworkBytes(networks map[string]dockerStatsNetwork) (uint64, uint64) {
	var rx, tx uint64
	for _, counters := range networks {
		rx = addUint64Saturating(rx, counters.RxBytes)
		tx = addUint64Saturating(tx, counters.TxBytes)
	}
	return rx, tx
}

func (a *Agent) verifyContainerResourceLimitsApplied(ctx context.Context, name string, expected dockerHostConfig) error {
	actual, err := a.inspectContainerHostConfig(ctx, name)
	if err != nil {
		return fmt.Errorf("failed to verify container resource limits: %w", err)
	}

	if expected.Memory > 0 && actual.Memory != expected.Memory {
		return fmt.Errorf(
			"container memory limit mismatch: expected %d MB, got %.0f MB",
			expected.Memory/(1024*1024),
			hostConfigMemoryLimitMb(actual),
		)
	}

	if expected.CpuPeriod > 0 && expected.CpuQuota > 0 {
		expectedPercent := hostConfigCpuLimitPercent(expected)
		actualPercent := hostConfigCpuLimitPercent(actual)
		if !almostEqualFloat64(actualPercent, expectedPercent, 0.05) {
			return fmt.Errorf(
				"container cpu limit mismatch: expected %.2f cores, got %.2f cores",
				expectedPercent/100.0,
				actualPercent/100.0,
			)
		}
	}

	return nil
}

func (a *Agent) runStructuredContainerWithCompatUser(ctx context.Context, name string, req dockerContainerCreateRequest) error {
	dockerName := dockerContainerName(name)
	cleanupFailedContainer := func(err error) error {
		return err
	}

	if err := a.docker.runContainerAPI(ctx, dockerName, req); err != nil {
		if len(req.Cmd) > 0 && isMissingRuntimeExecutableStartError(err) {
			imgCfg := a.inspectRuntimeImageConfig(ctx, req.Image)
			if imageConfigHasRuntimeDefault(imgCfg) {
				fmt.Printf("[docker] startup command failed for %s (%v); retrying with image default entrypoint/cmd\n", name, err)
				retryReq := req
				retryReq.Cmd = nil
				retryReq.Entrypoint = nil
				if workdir := normalizeContainerWorkdir(imgCfg.Workdir); workdir != "" {
					retryReq.WorkingDir = workdir
				}
				if retryErr := a.docker.runContainerAPI(ctx, dockerName, retryReq); retryErr != nil {
					return retryErr
				}
				if healthErr := a.verifyContainerHealthyAfterStart(ctx, name); healthErr != nil {
					return cleanupFailedContainer(healthErr)
				}
				if limitErr := a.verifyContainerResourceLimitsApplied(ctx, name, retryReq.HostConfig); limitErr != nil {
					return limitErr
				}
				return nil
			}
		}
		return err
	}
	if err := a.verifyContainerHealthyAfterStart(ctx, name); err != nil {
		forcedUser := strings.TrimSpace(req.User)
		if forcedUser == "" {
			return cleanupFailedContainer(err)
		}

		logCtx, logCancel := context.WithTimeout(context.Background(), 5*time.Second)
		logsOut := a.recentContainerLogs(logCtx, name, 80)
		logCancel()
		if !isImmediateStartupPermissionIssue(logsOut) {
			return cleanupFailedContainer(err)
		}

		fmt.Printf("[docker] startup failed under forced user %s for %s; retrying with image default user\n", forcedUser, name)
		a.docker.rmForce(ctx, name)

		retryReq := req
		retryReq.User = ""
		if err := a.docker.runContainerAPI(ctx, dockerName, retryReq); err != nil {
			return err
		}
		if err := a.verifyContainerHealthyAfterStart(ctx, name); err != nil {
			return cleanupFailedContainer(err)
		}
		if err := a.verifyContainerResourceLimitsApplied(ctx, name, retryReq.HostConfig); err != nil {
			return err
		}
		return nil
	}
	if err := a.verifyContainerResourceLimitsApplied(ctx, name, req.HostConfig); err != nil {
		return err
	}
	return nil
}

func (a *Agent) verifyContainerHealthyAfterStart(ctx context.Context, name string) error {
	deadline := time.Now().Add(4 * time.Second)
	for {
		time.Sleep(500 * time.Millisecond)

		statusOut, _, _, _ := a.docker.runCollect(ctx, "inspect", "--format", "{{.State.Status}} {{.State.ExitCode}}", name)
		statusParts := strings.Fields(strings.TrimSpace(statusOut))
		if len(statusParts) == 0 {
			if time.Now().Before(deadline) {
				continue
			}
			return nil
		}

		status := statusParts[0]
		exitStr := "0"
		if len(statusParts) >= 2 {
			exitStr = statusParts[1]
		}

		switch status {
		case "running":
			return nil
		case "created":
			if time.Now().Before(deadline) {
				continue
			}
			return nil
		case "restarting":
			logsOut := a.recentContainerLogs(ctx, name, 50)
			fmt.Printf("[docker] container %s is restarting after startup; leaving restart policy active and not blocking start. Last logs:\n%s\n", name, logsOut)
			return nil
		case "exited", "dead":
			logsOut := a.recentContainerLogs(ctx, name, 50)
			fmt.Printf("[docker] container exited after startup. Last logs:\n%s\n", logsOut)
			logSummary := strings.TrimSpace(logsOut)
			if len(logSummary) > 1800 {
				logSummary = logSummary[len(logSummary)-1800:]
			}
			if logSummary != "" {
				logSummary = "; last logs: " + logSummary
			}
			return fmt.Errorf("container exited immediately with code %s%s", exitStr, logSummary)
		default:
			if time.Now().Before(deadline) {
				continue
			}
			return nil
		}
	}
}

func customBootstrapFailureReason(logs string) string {
	lower := strings.ToLower(strings.TrimSpace(logs))
	if lower == "" {
		return ""
	}

	if strings.Contains(lower, "state is 0x202 after update job") &&
		(strings.Contains(lower, "srcds_run: no such file or directory") ||
			strings.Contains(lower, "tar:") ||
			strings.Contains(lower, "server.cfg: no such file or directory")) {
		return "SteamCMD/app install failed and the expected server files are missing"
	}

	fatalPatterns := []string{
		"executable file not found",
		"error during container init",
		"stat ",
		"no such file or directory",
		"permission denied",
		"error is not recoverable",
		"cannot open: no such file or directory",
		"is not a directory",
		"srcds_run: no such file or directory",
		"server.cfg: no such file or directory",
	}
	score := 0
	primary := ""
	for _, line := range strings.Split(lower, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, pattern := range fatalPatterns {
			if strings.Contains(line, pattern) {
				score++
				if primary == "" {
					primary = strings.TrimSpace(line)
				}
				break
			}
		}
	}
	if score >= 4 {
		if primary != "" {
			return primary
		}
		return "repeated fatal startup errors detected"
	}
	return ""
}

func logsSuggestBootstrapInProgress(logs string) bool {
	lower := strings.ToLower(strings.TrimSpace(logs))
	if lower == "" {
		return false
	}
	patterns := []string{
		"downloading update",
		"installing update",
		"extracting package",
		"steam console client",
		"steamcmd",
		"app_update",
		"installing",
		"extracting",
		"downloading",
		"npm install",
		"pip install",
		"apt-get",
		"apk add",
		"composer install",
	}
	for _, pattern := range patterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

func summarizeStartupLogs(logs string, maxLen int) string {
	clean := strings.TrimSpace(logs)
	if clean == "" {
		return ""
	}
	if maxLen <= 0 {
		maxLen = 1800
	}
	if len(clean) > maxLen {
		clean = clean[len(clean)-maxLen:]
	}
	return clean
}

type boundedLogWriter struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newBoundedLogWriter(max int) *boundedLogWriter {
	if max <= 0 {
		max = 64 * 1024
	}
	return &boundedLogWriter{max: max}
}

func (w *boundedLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		w.buf = append([]byte(nil), w.buf[len(w.buf)-w.max:]...)
	}
	return len(p), nil
}

func (w *boundedLogWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(append([]byte(nil), w.buf...))
}

type setupConsoleWriter struct {
	agent   *Agent
	name    string
	prefix  string
	mu      sync.Mutex
	partial string
}

func newSetupConsoleWriter(agent *Agent, name, prefix string) *setupConsoleWriter {
	prefix = strings.TrimSuffix(strings.TrimSpace(prefix), ":")
	if prefix != "stderr" {
		prefix = "stdout"
	}
	return &setupConsoleWriter{agent: agent, name: name, prefix: prefix}
}

func (w *setupConsoleWriter) Write(p []byte) (int, error) {
	if w == nil || w.agent == nil || w.name == "" {
		return len(p), nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	data := w.partial + string(p)
	data = strings.ReplaceAll(data, "\r\n", "\n")
	data = strings.ReplaceAll(data, "\r", "\n")
	parts := strings.Split(data, "\n")
	if strings.HasSuffix(data, "\n") {
		w.partial = ""
	} else {
		w.partial = parts[len(parts)-1]
		parts = parts[:len(parts)-1]
	}
	for _, line := range parts {
		w.agent.emitSetupLog(w.name, w.prefix, line)
	}
	return len(p), nil
}

func (w *setupConsoleWriter) Flush() {
	if w == nil || w.agent == nil || w.name == "" {
		return
	}
	w.mu.Lock()
	line := w.partial
	w.partial = ""
	w.mu.Unlock()
	if strings.TrimSpace(line) != "" {
		w.agent.emitSetupLog(w.name, w.prefix, line)
	}
}

func normalizeTemplateInstallConfig(raw *templateInstallConfig) *templateInstallConfig {
	if raw == nil {
		return nil
	}
	script := strings.TrimSpace(strings.ReplaceAll(raw.Script, "\r\n", "\n"))
	script = strings.ReplaceAll(script, "\r", "\n")
	if script == "" {
		return nil
	}
	if len(script) > 262144 {
		script = script[:262144]
	}

	image := strings.TrimSpace(raw.Image)
	if image == "" {
		image = strings.TrimSpace(raw.Container)
	}
	if image == "" {
		image = "ghcr.io/pterodactyl/installers:debian"
	}
	if strings.ContainsAny(image, " \t\r\n\x00") {
		return nil
	}

	mountPath := normalizeContainerWorkdir(raw.MountPath)
	if mountPath == "" {
		mountPath = normalizeContainerWorkdir(raw.ContainerPath)
	}
	if mountPath == "" {
		mountPath = "/mnt/server"
	}
	runtimePath := normalizeContainerWorkdir(raw.RuntimePath)
	if runtimePath == "" {
		runtimePath = "/home/container"
	}
	minimumStorageMb := raw.MinimumStorageMb
	if minimumStorageMb < 0 {
		minimumStorageMb = 0
	}
	timeoutSeconds := raw.TimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = 1800
	}
	if timeoutSeconds < 60 {
		timeoutSeconds = 60
	}
	if timeoutSeconds > 7200 {
		timeoutSeconds = 7200
	}

	env := map[string]string{}
	for key, value := range raw.Env {
		envKey := strings.TrimSpace(key)
		if envKey == "" || strings.ContainsAny(envKey, "\x00=\r\n") {
			continue
		}
		env[envKey] = value
	}

	clean := &templateInstallConfig{
		Image:            image,
		Script:           script,
		MountPath:        mountPath,
		RuntimePath:      runtimePath,
		TimeoutSeconds:   timeoutSeconds,
		MinimumStorageMb: minimumStorageMb,
		AllowEmpty:       raw.AllowEmpty || raw.AllowEmptyResult,
	}
	if len(env) > 0 {
		clean.Env = env
	}
	return clean
}

func dockerTemplateInstall(cfg *dockerTemplateConfig) *templateInstallConfig {
	if cfg == nil {
		return nil
	}
	if install := normalizeTemplateInstallConfig(cfg.Install); install != nil {
		return install
	}
	return normalizeTemplateInstallConfig(cfg.Installation)
}

func installConfigFromRuntime(runtimeObj map[string]any) *templateInstallConfig {
	if runtimeObj == nil {
		return nil
	}
	raw, ok := runtimeObj["install"]
	if !ok || raw == nil {
		return nil
	}
	if install, ok := raw.(*templateInstallConfig); ok {
		return normalizeTemplateInstallConfig(install)
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var install templateInstallConfig
	if err := json.Unmarshal(b, &install); err != nil {
		return nil
	}
	return normalizeTemplateInstallConfig(&install)
}

func runtimeImageRef(runtimeObj map[string]any) string {
	if runtimeObj == nil {
		return ""
	}
	image := strings.TrimSpace(fmt.Sprint(runtimeObj["image"]))
	if image == "" || image == "<nil>" {
		return ""
	}
	tag := strings.TrimSpace(fmt.Sprint(runtimeObj["tag"]))
	if tag == "" || tag == "<nil>" {
		tag = "latest"
	}
	return image + ":" + tag
}

func safeInstallerContainerName(serverName string) string {
	base := sanitizeName(serverName)
	if base == "" {
		base = "server"
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	name := "adpanel-install-" + base + "-" + suffix
	if len(name) > 120 {
		name = name[:120]
	}
	return name
}

func adpanelStartupDefaultEnv(name string, hostPort int, resources *resourceLimits, startup string) map[string]string {
	env := map[string]string{
		"TZ":                        "Etc/UTC",
		"SERVER_IP":                 "0.0.0.0",
		"P_SERVER_LOCATION":         "ADPanel",
		"P_SERVER_ALLOCATION_LIMIT": "0",
		"P_SERVER_UUID":             name,
		"P_SERVER_UUID_SHORT":       name,
	}
	if hostPort > 0 {
		env["SERVER_PORT"] = fmt.Sprint(hostPort)
		env["PORT"] = fmt.Sprint(hostPort)
	}
	if resources != nil && resources.RamMb != nil && *resources.RamMb > 0 {
		env["SERVER_MEMORY"] = fmt.Sprint(*resources.RamMb)
	}
	if strings.TrimSpace(startup) != "" {
		env["STARTUP"] = startup
	}
	return env
}

func mergeStringEnv(dst map[string]string, src map[string]string) map[string]string {
	if dst == nil {
		dst = map[string]string{}
	}
	for key, value := range src {
		envKey := strings.TrimSpace(key)
		if envKey == "" || strings.ContainsAny(envKey, "\x00=\r\n") {
			continue
		}
		dst[envKey] = value
	}
	return dst
}

func startupTemplateFromRuntime(runtimeObj map[string]any) string {
	if runtimeObj == nil {
		return ""
	}
	for _, key := range []string{"adpanelStartup", "adpanel_startup", "pterodactylStartup", "pterodactyl_startup"} {
		value := strings.TrimSpace(fmt.Sprint(runtimeObj[key]))
		if value != "" && value != "<nil>" {
			return value
		}
	}
	return ""
}

func startupTemplateFromDockerConfig(cfg *dockerTemplateConfig) string {
	if cfg == nil {
		return ""
	}
	if startup := strings.TrimSpace(cfg.ADPanelStartup); startup != "" {
		return startup
	}
	if startup := strings.TrimSpace(cfg.ADPanelStartupSnake); startup != "" {
		return startup
	}
	if startup := strings.TrimSpace(cfg.LegacyStartup); startup != "" {
		return startup
	}
	return strings.TrimSpace(cfg.LegacyStartupSnake)
}

func startupDisplayFromDockerConfig(cfg *dockerTemplateConfig) string {
	if cfg == nil {
		return ""
	}
	for _, value := range []string{
		cfg.StartupDisplay,
		cfg.StartupDisplaySnake,
		cfg.StartupPreview,
		cfg.StartupPreviewSnake,
		cfg.ImageDefaultStartup,
		cfg.ImageDefaultStartupSnake,
	} {
		if display := strings.TrimSpace(value); display != "" {
			return display
		}
	}
	return ""
}

func javaVersionFromDockerConfig(cfg *dockerTemplateConfig) string {
	if cfg == nil {
		return ""
	}
	if version := strings.TrimSpace(cfg.JavaVersion); version != "" {
		return version
	}
	return strings.TrimSpace(cfg.Java)
}

func freeDiskMb(path string) int64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0
	}
	return int64(stat.Bavail) * int64(stat.Bsize) / (1024 * 1024)
}

func renderADPanelStartupTemplate(startup string, env map[string]string, hostPort int) string {
	rendered := strings.TrimSpace(startup)
	if rendered == "" {
		return ""
	}
	values := map[string]string{}
	for key, value := range env {
		values[strings.TrimSpace(key)] = value
	}
	if hostPort > 0 {
		values["SERVER_PORT"] = fmt.Sprint(hostPort)
		values["PORT"] = fmt.Sprint(hostPort)
	}

	replacer := regexp.MustCompile(`\{\{\s*([A-Za-z0-9_.-]+)\s*\}\}`)
	rendered = replacer.ReplaceAllStringFunc(rendered, func(token string) string {
		match := replacer.FindStringSubmatch(token)
		if len(match) != 2 {
			return token
		}
		key := strings.TrimSpace(match[1])
		switch key {
		case "server.build.default.port":
			if hostPort > 0 {
				return fmt.Sprint(hostPort)
			}
			return token
		}
		if strings.HasPrefix(key, "env.") {
			key = strings.TrimPrefix(key, "env.")
		}
		if value, ok := values[key]; ok {
			return value
		}
		return token
	})
	return rendered
}

func (a *Agent) emitSetupLog(name, prefix, message string) {
	if a == nil || a.logs == nil {
		return
	}
	name = sanitizeName(name)
	if name == "" {
		return
	}
	prefix = strings.TrimSuffix(strings.TrimSpace(prefix), ":")
	if prefix != "stderr" {
		prefix = "stdout"
	}
	message = strings.ReplaceAll(message, "\r\n", "\n")
	message = strings.ReplaceAll(message, "\r", "\n")
	t := a.logs.getOrCreate(name)
	for _, line := range strings.Split(message, "\n") {
		line = strings.TrimRight(line, " \t")
		plain := strings.TrimSpace(ansiSGRRE.ReplaceAllString(line, ""))
		if plain == "" {
			continue
		}
		a.logs.sendToClients(t, prefix+":[ADPanel Setup] "+line)
	}
}

func (a *Agent) checkTemplateInstallPreflight(name, serverDir string, install *templateInstallConfig) error {
	install = normalizeTemplateInstallConfig(install)
	if install == nil {
		return nil
	}
	if err := ensureDir(serverDir); err != nil {
		return err
	}
	ensureVolumeWritable(serverDir)
	if install.MinimumStorageMb > 0 {
		freeMb := freeDiskMb(serverDir)
		if freeMb > 0 && freeMb < int64(install.MinimumStorageMb) {
			return fmt.Errorf("not enough free disk for installer: template requires at least %d MB, host has %d MB free", install.MinimumStorageMb, freeMb)
		}
	}
	return nil
}

func (a *Agent) pullSetupImage(name, serverDir, imageRef, label string, timeout time.Duration) error {
	imageRef = strings.TrimSpace(imageRef)
	if imageRef == "" {
		return nil
	}
	label = strings.TrimSpace(label)
	if label == "" {
		label = "Docker"
	}
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	a.updateSetupState(name, serverDir, "pulling", fmt.Sprintf("Pulling %s image %s", label, imageRef))
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	err := a.docker.ensureImageAvailableAPI(ctx, imageRef)
	cancel()
	if err != nil {
		return fmt.Errorf("%s image pull failed: %w", label, err)
	}
	a.emitSetupLog(name, "stdout", fmt.Sprintf("%s image ready: %s", label, imageRef))
	return nil
}

func installerScriptPreferredShell(script string) string {
	clean := strings.TrimSpace(strings.ReplaceAll(script, "\r\n", "\n"))
	if clean == "" {
		return "sh"
	}

	if strings.HasPrefix(clean, "#!") {
		firstLine := strings.TrimSpace(strings.SplitN(clean, "\n", 2)[0])
		firstLine = strings.TrimSpace(strings.TrimPrefix(firstLine, "#!"))
		for _, token := range strings.Fields(firstLine) {
			base := strings.ToLower(path.Base(strings.TrimSpace(token)))
			switch base {
			case "bash", "zsh", "ksh":
				return base
			case "sh", "ash", "dash":
				return "sh"
			}
		}
	}

	lower := strings.ToLower(clean)
	if strings.Contains(lower, "[[") ||
		strings.Contains(lower, "function ") ||
		strings.Contains(lower, "source ") ||
		strings.Contains(lower, "set -o pipefail") {
		return "bash"
	}
	return "sh"
}

func installerShellCommandFlag(shell string) string {
	switch strings.ToLower(path.Base(strings.TrimSpace(shell))) {
	case "bash", "zsh", "ksh":
		return "-lc"
	default:
		return "-c"
	}
}

func installerDefaultCommandArgs(entrypoint []string, script string) []string {
	ep := trimRuntimeProcessArgs(entrypoint)
	if len(ep) == 0 {
		shell := installerScriptPreferredShell(script)
		return []string{shell, installerShellCommandFlag(shell), script}
	}
	execIndex := 0
	if strings.EqualFold(path.Base(ep[0]), "env") && len(ep) > 1 {
		execIndex = 1
	}
	base := strings.ToLower(path.Base(ep[execIndex]))
	if base == "bash" || base == "zsh" || base == "ksh" || base == "sh" || base == "ash" || base == "dash" {
		if len(ep) > execIndex+1 && strings.HasPrefix(ep[execIndex+1], "-") && strings.Contains(ep[execIndex+1], "c") {
			return []string{script}
		}
		return []string{installerShellCommandFlag(base), script}
	}
	return []string{script}
}

func (a *Agent) runTemplateInstall(name, serverDir string, install *templateInstallConfig, runtimeObj map[string]any, hostPort int, resources *resourceLimits) error {
	install = normalizeTemplateInstallConfig(install)
	if install == nil {
		return nil
	}
	if err := ensureDir(serverDir); err != nil {
		return err
	}
	ensureVolumeWritable(serverDir)

	startup := startupTemplateFromRuntime(runtimeObj)
	env := adpanelStartupDefaultEnv(name, hostPort, resources, startup)
	env = mergeStringEnv(env, runtimeEnvMap(runtimeObj["env"]))
	env = mergeStringEnv(env, install.Env)
	tpl := strings.ToLower(strings.TrimSpace(fmt.Sprint(runtimeObj["providerId"])))
	applyRuntimeHostPortEnv(tpl, env, hostPort)
	renderRuntimeEnvTemplates(tpl, env, hostPort)
	if startup != "" {
		env["STARTUP"] = renderADPanelStartupTemplate(startup, env, hostPort)
	}
	if err := a.checkTemplateInstallPreflight(name, serverDir, install); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(install.TimeoutSeconds)*time.Second)
	defer cancel()

	a.emitSetupLog(name, "stdout", "Preparing template installer")
	if err := a.docker.ensureImageAvailableAPI(ctx, install.Image); err != nil {
		return fmt.Errorf("installer image pull failed: %w", err)
	}
	a.emitSetupLog(name, "stdout", "Running template installer in "+install.Image)
	imgCfg := a.inspectRuntimeImageConfig(ctx, install.Image)
	installerCmdArgs := installerDefaultCommandArgs(imgCfg.Entrypoint, install.Script)

	containerName := safeInstallerContainerName(name)
	defer a.docker.rmForce(context.Background(), containerName)

	args := []string{
		"run", "--rm",
		"--name", containerName,
		"-v", fmt.Sprintf("%s:%s", serverDir, install.MountPath),
		"-w", install.MountPath,
	}
	for key, value := range env {
		args = append(args, "-e", fmt.Sprintf("%s=%s", key, value))
	}
	args = append(args, install.Image)
	args = append(args, installerCmdArgs...)

	logs := newBoundedLogWriter(96 * 1024)
	stdoutLog := newSetupConsoleWriter(a, name, "stdout")
	stderrLog := newSetupConsoleWriter(a, name, "stderr")
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = io.MultiWriter(logs, stdoutLog)
	cmd.Stderr = io.MultiWriter(logs, stderrLog)
	err := cmd.Run()
	stdoutLog.Flush()
	stderrLog.Flush()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("template installer timed out after %ds; last logs: %s", install.TimeoutSeconds, summarizeStartupLogs(logs.String(), 2400))
		}
		return fmt.Errorf("template installer failed: %w; last logs: %s", err, summarizeStartupLogs(logs.String(), 2400))
	}

	ensureVolumeWritable(serverDir)
	if count := serverDirVisibleFileCount(serverDir); count == 0 && !install.AllowEmpty {
		return fmt.Errorf("template installer completed but did not create any visible server files in %s; last logs: %s", install.MountPath, summarizeStartupLogs(logs.String(), 2400))
	} else if count == 0 {
		a.emitSetupLog(name, "stdout", "Installer completed without visible files; continuing because allowEmpty is enabled")
	}
	a.emitSetupLog(name, "stdout", "Template installer completed")
	fmt.Printf("[installer] template install completed for %s using %s into %s\n", name, install.Image, install.MountPath)
	return nil
}

func (a *Agent) updateSetupState(name, serverDir, status, message string) {
	meta := a.loadMeta(serverDir)
	if meta == nil {
		meta = map[string]any{}
	}
	status = strings.TrimSpace(status)
	message = strings.TrimSpace(message)
	if status == "" {
		delete(meta, "setupStatus")
	} else {
		meta["setupStatus"] = status
	}
	if message == "" {
		delete(meta, "setupMessage")
	} else {
		meta["setupMessage"] = message
	}
	meta["setupUpdatedAt"] = time.Now().UnixMilli()
	if err := a.saveMeta(serverDir, meta); err != nil {
		fmt.Printf("[setup] failed to update setup state for %s: %v\n", name, err)
	}
	if status != "" {
		line := status
		if message != "" {
			line += ": " + message
		}
		prefix := "stdout"
		if strings.EqualFold(status, "failed") {
			prefix = "stderr"
		}
		a.emitSetupLog(name, prefix, line)
	}
}

func (a *Agent) prepareMinecraftCreateSetup(req createReq, name, serverDir string, meta map[string]any) bool {
	runtimeObj, _ := meta["runtime"].(map[string]any)
	imageRef := runtimeImageRef(runtimeObj)
	if imageRef != "" {
		if err := a.pullSetupImage(name, serverDir, imageRef, "runtime", 10*time.Minute); err != nil {
			a.updateSetupState(name, serverDir, "failed", err.Error())
			fmt.Printf("[setup] Minecraft runtime image pull failed for %s: %v\n", name, err)
			return false
		}
	}
	a.updateSetupState(name, serverDir, "queued", "Docker image ready; Minecraft setup will continue in the background")
	return true
}

func (a *Agent) prepareCustomCreateSetup(req createReq, name, serverDir string, meta map[string]any) bool {
	runtimeObj, _ := meta["runtime"].(map[string]any)
	if runtimeObj == nil {
		runtimeObj = map[string]any{}
	}
	a.updateSetupState(name, serverDir, "preparing", "Preparing Docker template")
	if install := installConfigFromRuntime(runtimeObj); install != nil {
		if err := a.checkTemplateInstallPreflight(name, serverDir, install); err != nil {
			a.updateSetupState(name, serverDir, "failed", err.Error())
			fmt.Printf("[setup] Template preflight failed for %s: %v\n", name, err)
			return false
		}
		if err := a.pullSetupImage(name, serverDir, install.Image, "installer", 10*time.Minute); err != nil {
			a.updateSetupState(name, serverDir, "failed", err.Error())
			fmt.Printf("[setup] Template installer image pull failed for %s: %v\n", name, err)
			return false
		}
	}
	if imageRef := runtimeImageRef(runtimeObj); imageRef != "" {
		if err := a.pullSetupImage(name, serverDir, imageRef, "runtime", 10*time.Minute); err != nil {
			a.updateSetupState(name, serverDir, "failed", err.Error())
			fmt.Printf("[setup] Runtime image pull failed for %s: %v\n", name, err)
			return false
		}
	}
	a.updateSetupState(name, serverDir, "queued", "Docker images ready; setup will continue in the background")
	return true
}

func (a *Agent) finishMinecraftCreateSetup(req createReq, name, serverDir string, meta map[string]any, hostPort int) {
	fork := strings.TrimSpace(req.MCFork)
	if fork == "" {
		fork = "paper"
	}
	ver := strings.TrimSpace(req.MCVersion)
	if ver == "" {
		ver = "1.21.8"
	}

	a.updateSetupState(name, serverDir, "downloading", "Downloading Minecraft server files")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	jarURL := getMinecraftJarURL(ctx, a.dl, fork, ver, a.debug)
	cancel()

	ctxD, cancelD := context.WithTimeout(context.Background(), 30*time.Minute)
	if err := a.dl.downloadToFile(ctxD, jarURL, filepath.Join(serverDir, "server.jar")); err != nil {
		cancelD()
		a.updateSetupState(name, serverDir, "failed", err.Error())
		fmt.Printf("[setup] Minecraft download failed for %s: %v\n", name, err)
		return
	}
	cancelD()

	enforceServerProps(serverDir)

	if req.ImportUrl != "" {
		a.updateSetupState(name, serverDir, "importing", "Importing archive")
		if err := a.downloadAndExtractArchiveSync(name, serverDir, req.ImportUrl); err != nil {
			a.updateSetupState(name, serverDir, "failed", "archive import failed: "+err.Error())
			fmt.Printf("[setup] Minecraft archive import failed for %s: %v\n", name, err)
			return
		}
	}

	if req.AutoStart {
		a.updateSetupState(name, serverDir, "starting", "Starting container")
		ctxStart, cancelStart := context.WithTimeout(context.Background(), 240*time.Second)
		err := a.startMinecraftContainer(ctxStart, name, serverDir, hostPort, req.Resources, nil)
		cancelStart()
		if err != nil {
			a.updateSetupState(name, serverDir, "failed", err.Error())
			fmt.Printf("[setup] Minecraft startup failed for %s: %v\n", name, err)
			return
		}
		a.ensureLiveLogs(name)
	}

	a.updateSetupState(name, serverDir, "ready", "Server setup complete")
	auditLog.Log("server_setup", "127.0.0.1", "", name, "setup", "success", map[string]any{"template": req.Template})
}

func (a *Agent) finishCustomCreateSetup(req createReq, name, serverDir string, meta map[string]any, hostPort int) {
	runtimeObj, _ := meta["runtime"].(map[string]any)
	if runtimeObj == nil {
		runtimeObj = map[string]any{}
	}
	if install := installConfigFromRuntime(runtimeObj); install != nil {
		a.updateSetupState(name, serverDir, "installing", "Installing server files")
		if err := a.runTemplateInstall(name, serverDir, install, runtimeObj, hostPort, req.Resources); err != nil {
			a.updateSetupState(name, serverDir, "failed", err.Error())
			fmt.Printf("[setup] Template install failed for %s: %v\n", name, err)
			return
		}
	}

	if req.ImportUrl != "" {
		a.updateSetupState(name, serverDir, "importing", "Importing archive")
		if err := a.downloadAndExtractArchiveSync(name, serverDir, req.ImportUrl); err != nil {
			a.updateSetupState(name, serverDir, "failed", "archive import failed: "+err.Error())
			fmt.Printf("[setup] Archive import failed for %s: %v\n", name, err)
			return
		}
	}

	template := strings.ToLower(strings.TrimSpace(req.Template))
	switch template {
	case "python":
		scaffoldPython(serverDir, defaultStartFileForType(template))
	case "nodejs", "discord-bot":
		scaffoldNode(serverDir, defaultStartFileForType(template), hostPort)
	}

	hasImage := req.Docker != nil && strings.TrimSpace(req.Docker.Image) != ""
	if req.AutoStart && hasImage {
		a.updateSetupState(name, serverDir, "starting", "Starting container")
		ctxStart, cancelStart := context.WithTimeout(context.Background(), 240*time.Second)
		var err error
		if isManagedProcessRuntime(template) {
			err = a.startRuntimeContainer(ctxStart, name, serverDir, meta, hostPort, req.Resources, nil)
		} else {
			err = a.startCustomDockerContainer(ctxStart, name, serverDir, meta, hostPort, req.Resources)
		}
		cancelStart()
		if err != nil {
			a.updateSetupState(name, serverDir, "failed", err.Error())
			fmt.Printf("[setup] Container startup failed for %s: %v\n", name, err)
			return
		}
		a.ensureLiveLogs(name)
	}

	a.updateSetupState(name, serverDir, "ready", "Server setup complete")
	auditLog.Log("server_setup", "127.0.0.1", "", name, "setup", "success", map[string]any{"template": req.Template})
}

func (a *Agent) monitorCustomContainerBootstrap(ctx context.Context, name, imageRef string) error {
	started := time.Now()
	defaultWindow := 18 * time.Second
	bootstrapWindow := 115 * time.Second
	if isGameServerDockerImage(imageRef) {
		bootstrapWindow = 150 * time.Second
	}

	seenBootstrap := false
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			logsOut := a.recentContainerLogs(logCtx, name, 160)
			cancel()
			return fmt.Errorf("container bootstrap did not become stable before timeout; last logs: %s", summarizeStartupLogs(logsOut, 1800))
		case <-ticker.C:
		}

		statusOut, _, _, _ := a.docker.runCollect(ctx, "inspect", "--format", "{{.State.Status}} {{.State.ExitCode}}", name)
		statusParts := strings.Fields(strings.TrimSpace(statusOut))
		status := ""
		exitStr := "0"
		if len(statusParts) > 0 {
			status = statusParts[0]
		}
		if len(statusParts) > 1 {
			exitStr = statusParts[1]
		}

		logsOut := a.recentContainerLogs(ctx, name, 160)
		if logsSuggestBootstrapInProgress(logsOut) {
			seenBootstrap = true
		}
		if reason := customBootstrapFailureReason(logsOut); reason != "" {
			return fmt.Errorf("container bootstrap failed: %s; last logs: %s", reason, summarizeStartupLogs(logsOut, 1800))
		}

		switch status {
		case "restarting":
			if time.Since(started) >= defaultWindow {
				fmt.Printf("[docker] %s is restarting during bootstrap; not blocking startup.\n", name)
				return nil
			}
			continue
		case "exited", "dead":
			if status == "" {
				status = "unknown"
			}
			return fmt.Errorf("container stopped during bootstrap (%s, exit code %s); last logs: %s", status, exitStr, summarizeStartupLogs(logsOut, 1800))
		case "running":
			elapsed := time.Since(started)
			if seenBootstrap {
				if elapsed >= bootstrapWindow {
					return nil
				}
				continue
			}
			if elapsed >= defaultWindow {
				return nil
			}
		case "created", "":
			if time.Since(started) > defaultWindow {
				return nil
			}
		default:
			if time.Since(started) > defaultWindow {
				return nil
			}
		}
	}
}

func (a *Agent) startMinecraftContainer(ctx context.Context, name, serverDir string, hostPort int, resources *resourceLimits, extraEnv map[string]string) error {
	a.docker.rmForce(ctx, name)
	meta := a.loadMeta(serverDir)
	runtimeObj, _ := meta["runtime"].(map[string]any)
	if runtimeObj == nil {
		runtimeObj = make(map[string]any)
	}

	ensureVolumeWritable(serverDir)
	uid, gid := statUIDGID(serverDir)

	commandTemplate := sanitizeRuntimeCommand(runtimeObj["command"])
	image := strings.TrimSpace(fmt.Sprint(runtimeObj["image"]))
	tag := strings.TrimSpace(fmt.Sprint(runtimeObj["tag"]))
	rawJavaVersion := strings.TrimSpace(fmt.Sprint(runtimeObj["javaVersion"]))
	selectedJavaVersion := normalizeMinecraftJavaVersion(rawJavaVersion)
	if rawJavaVersion != "" && rawJavaVersion != "<nil>" && selectedJavaVersion == "" {
		return fmt.Errorf("unsupported minecraft java version: %s", rawJavaVersion)
	}
	if selectedJavaVersion != "" {
		image = defaultMinecraftProcessImage
		tag = minecraftJavaTag(selectedJavaVersion)
		runtimeObj["image"] = image
		runtimeObj["tag"] = tag
		runtimeObj["javaVersion"] = selectedJavaVersion
	}
	useLegacyImageMode := false

	if image == "" || image == "<nil>" {
		if commandTemplate == "" {
			image = "itzg/minecraft-server"
			tag = "latest"
			useLegacyImageMode = true
		} else {
			image = defaultMinecraftProcessImage
			tag = defaultMinecraftProcessTag
		}
	}
	if tag == "" || tag == "<nil>" {
		if strings.EqualFold(image, "itzg/minecraft-server") && commandTemplate == "" {
			tag = "latest"
			useLegacyImageMode = true
		} else {
			tag = defaultMinecraftProcessTag
		}
	}
	if strings.EqualFold(image, "itzg/minecraft-server") && commandTemplate == "" {
		useLegacyImageMode = true
	}

	imageRef := image + ":" + tag
	binds, dataDir := a.resolveStructuredBinds("minecraft", runtimeObj, serverDir)
	workdir := normalizeContainerWorkdir(runtimeObj["workdir"])
	if workdir == "" {
		workdir = dataDir
	}
	startFile := strings.TrimSpace(fmt.Sprint(meta["start"]))
	if startFile == "" || startFile == "<nil>" {
		startFile = "server.jar"
	}

	env := runtimeEnvMap(runtimeObj["env"])
	if env == nil {
		env = map[string]string{}
	}
	for k, v := range extraEnv {
		env[strings.TrimSpace(k)] = v
	}

	req := dockerContainerCreateRequest{
		Image:        imageRef,
		Tty:          containerTTYForHost(),
		OpenStdin:    true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		WorkingDir:   workdir,
	}

	if useLegacyImageMode {
		if _, ok := env["EULA"]; !ok {
			env["EULA"] = "TRUE"
		}
		if _, ok := env["TYPE"]; !ok {
			env["TYPE"] = "CUSTOM"
		}
		if _, ok := env["CUSTOM_SERVER"]; !ok {
			env["CUSTOM_SERVER"] = "/data/server.jar"
		}
		if _, ok := env["ENABLE_RCON"]; !ok {
			env["ENABLE_RCON"] = "false"
		}
		if _, ok := env["CREATE_CONSOLE_IN_PIPE"]; !ok {
			env["CREATE_CONSOLE_IN_PIPE"] = "true"
		}
		if _, ok := env["UID"]; !ok {
			env["UID"] = fmt.Sprint(uid)
		}
		if _, ok := env["GID"]; !ok {
			env["GID"] = fmt.Sprint(gid)
		}
	} else {
		if commandTemplate == "" {
			commandTemplate = defaultRuntimeCommandForType("minecraft", startFile, dataDir)
		}
		cmdArgs, err := buildRenderedRuntimeProcessArgs("minecraft", commandTemplate, dataDir, startFile, imageRef, hostPort, resources, meta)
		if err != nil {
			return err
		}
		req.User = a.structuredContainerUser(ctx, imageRef, uid, gid, runtimeObj)
		req.Cmd = cmdArgs
	}

	portBindings, exposedPorts := a.buildStructuredPortBindings(serverDir, meta, hostPort, 25565)
	req.ExposedPorts = exposedPorts
	req.HostConfig = buildStructuredHostConfig(
		binds,
		portBindings,
		resources,
		strings.TrimSpace(fmt.Sprint(runtimeObj["restart"])),
		false,
	)
	req.Env = envMapToList(env)

	if err := a.runStructuredContainerWithCompatUser(ctx, name, req); err != nil {
		return err
	}
	return nil
}

func (a *Agent) startRuntimeContainer(ctx context.Context, name, serverDir string, meta map[string]any, hostPort int, resources *resourceLimits, extraEnv map[string]string) error {
	a.docker.rmForce(ctx, name)

	runtimeObj, _ := meta["runtime"].(map[string]any)
	tpl := strings.ToLower(fmt.Sprint(meta["type"]))
	if tpl == "" {
		tpl = strings.ToLower(fmt.Sprint(runtimeObj["providerId"]))
	}
	isPython := tpl == "python"

	image := strings.TrimSpace(fmt.Sprint(runtimeObj["image"]))
	tag := strings.TrimSpace(fmt.Sprint(runtimeObj["tag"]))
	if image == "" {
		if isPython {
			image = "python"
		} else {
			image = "node"
		}
	}
	if tag == "" {
		if isPython {
			tag = "3.12-slim"
		} else {
			tag = "20-alpine"
		}
	}
	imageRef := image
	if tag != "" {
		imageRef = image + ":" + tag
	}
	ensureVolumeWritable(serverDir)

	effectivePort := 0
	if hostPort > 0 {
		effectivePort = hostPort
	}

	binds, dataDir := a.resolveStructuredBinds(tpl, runtimeObj, serverDir)
	workdir := normalizeContainerWorkdir(runtimeObj["workdir"])
	if workdir == "" {
		workdir = dataDir
	}

	env := runtimeEnvMap(runtimeObj["env"])
	if env == nil {
		env = map[string]string{}
	}
	for k, v := range extraEnv {
		env[strings.TrimSpace(k)] = v
	}
	if effectivePort > 0 {
		applyRuntimeHostPortEnv(tpl, env, effectivePort)
		renderRuntimeEnvTemplates(tpl, env, effectivePort)
	}

	startFile := strings.TrimSpace(fmt.Sprint(meta["start"]))
	if startFile == "" || startFile == "<nil>" {
		startFile = defaultStartFileForType(tpl)
	}

	commandTemplate := sanitizeRuntimeCommand(runtimeObj["command"])
	if commandTemplate == "" {
		commandTemplate = defaultRuntimeCommandForType(tpl, startFile, dataDir)
	}
	ensureRuntimeEntrypointScaffold(serverDir, tpl, commandTemplate, dataDir, workdir, startFile, effectivePort)
	cmdArgs, err := buildRenderedRuntimeProcessArgs(tpl, commandTemplate, dataDir, startFile, imageRef, effectivePort, resources, meta)
	if err != nil {
		return err
	}
	uid, gid := statUIDGID(serverDir)
	req := dockerContainerCreateRequest{
		Image:        imageRef,
		Env:          envMapToList(env),
		Cmd:          cmdArgs,
		WorkingDir:   workdir,
		User:         a.structuredContainerUser(ctx, imageRef, uid, gid, runtimeObj),
		Tty:          containerTTYForHost(),
		OpenStdin:    true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	}
	portBindings, exposedPorts := a.buildStructuredPortBindings(serverDir, meta, effectivePort, effectivePort)
	req.ExposedPorts = exposedPorts
	req.HostConfig = buildStructuredHostConfig(
		binds,
		portBindings,
		resources,
		strings.TrimSpace(fmt.Sprint(runtimeObj["restart"])),
		false,
	)

	if err := a.runStructuredContainerWithCompatUser(ctx, name, req); err != nil {
		return err
	}
	return nil
}

func matchesBlockedFlag(flag string, denylist map[string]bool) (bool, string) {
	if !strings.HasPrefix(flag, "--") || len(flag) <= 2 {
		return false, ""
	}
	for blocked := range denylist {
		if strings.HasPrefix(blocked, flag) {
			return true, blocked
		}
	}
	return false, ""
}

var legacyRawDockerStartupCommandsDisabled = true

func (a *Agent) executeStartupCommand(ctx context.Context, name, serverDir string, hostPort int, startupCmd string, resources *resourceLimits, meta map[string]any) error {
	if legacyRawDockerStartupCommandsDisabled {
		fmt.Printf("[security] blocked legacy raw docker startup command path for %s\n", name)
		return fmt.Errorf("raw docker startup commands are disabled; configure image, ports, volumes, env, and the in-container process separately")
	}
	if a.debug {
		fmt.Printf("[docker] executeStartupCommand called for %s\n", name)
		fmt.Printf("[docker] serverDir: %s\n", serverDir)
	}

	a.docker.rmForce(ctx, name)

	cmd := startupCmd
	cmd = strings.ReplaceAll(cmd, "{SERVER_NAME}", name)
	cmd = strings.ReplaceAll(cmd, "{PORT}", fmt.Sprint(hostPort))
	cmd = strings.ReplaceAll(cmd, "{DATA_DIR}", serverDir)
	cmd = strings.ReplaceAll(cmd, "{BOT_DIR}", serverDir)
	cmd = strings.ReplaceAll(cmd, "{SERVER_DIR}", serverDir)

	ramMb := 0
	cpuCores := float64(0)
	if resources != nil {
		if resources.RamMb != nil {
			ramMb = *resources.RamMb
		}
		if resources.CpuCores != nil {
			cpuCores = *resources.CpuCores
		}
	}
	cmd = strings.ReplaceAll(cmd, "{RAM_MB}", fmt.Sprint(ramMb))
	cmd = strings.ReplaceAll(cmd, "{CPU_CORES}", fmt.Sprintf("%.1f", cpuCores))

	runtimeObj, _ := meta["runtime"].(map[string]any)
	image := ""
	if v := runtimeObj["image"]; v != nil {
		image = strings.TrimSpace(fmt.Sprint(v))
		if image == "<nil>" {
			image = ""
		}
	}
	tag := ""
	if v := runtimeObj["tag"]; v != nil {
		tag = strings.TrimSpace(fmt.Sprint(v))
		if tag == "<nil>" {
			tag = ""
		}
	}
	if tag == "" {
		tag = "latest"
	}
	imageRef := image + ":" + tag
	cmd = strings.ReplaceAll(cmd, "{IMAGE}", imageRef)

	allowedHostPorts := make([]int, 0, 8)
	if port, ok := parseStrictPortValue(hostPort); ok {
		allowedHostPorts = append(allowedHostPorts, port)
	}
	if metaRes, ok := meta["resources"].(map[string]any); ok {
		for _, port := range runtimeIntSlice(metaRes["ports"]) {
			if port > 0 {
				allowedHostPorts = append(allowedHostPorts, port)
			}
		}
	}
	reservedHostPorts := make([]int, 0, 4)
	for _, pf := range a.loadPortForwards(serverDir) {
		if port, ok := parseStrictPortValue(pf.Public); ok {
			reservedHostPorts = append(reservedHostPorts, port)
		}
	}
	expectedImageRef := ""
	if image != "" {
		expectedImageRef = imageRef
	}

	cmd = strings.TrimSpace(cmd)
	if safetyViolation := validateDockerRunCommandSafety(cmd, allowedHostPorts, reservedHostPorts, expectedImageRef); safetyViolation != "" {
		return errors.New(safetyViolation)
	}
	cmdLower := strings.ToLower(cmd)
	if !strings.HasPrefix(cmdLower, "docker run") {
		return fmt.Errorf("startup command must start with 'docker run'")
	}

	argsStr := cmd[len("docker run"):]
	argsStr = strings.TrimSpace(argsStr)

	args := parseShellArgs(argsStr)

	dockerImage := ""
	skipNext := false
	flagsWithValue := map[string]bool{
		"-e": true, "--env": true, "-v": true, "--volume": true,
		"-p": true, "--publish": true, "-w": true, "--workdir": true,
		"--name": true, "-m": true, "--memory": true, "--cpus": true,
		"--memory-swap": true, "--memory-reservation": true, "--cpu-period": true, "--cpu-quota": true,
		"-u": true, "--user": true, "-h": true, "--hostname": true,
		"--network": true, "--net": true, "--restart": true,
		"-l": true, "--label": true, "--entrypoint": true,
		"--mount": true, "--tmpfs": true, "--device": true,
		"--pid": true, "--ipc": true, "--userns": true, "--uts": true, "--cgroupns": true,
		"--shm-size": true, "--ulimit": true, "--dns": true, "--dns-search": true,
		"--add-host": true, "--log-driver": true, "--log-opt": true,
		"--stop-signal": true, "--stop-timeout": true, "--health-cmd": true,
		"--health-interval": true, "--health-retries": true, "--health-timeout": true,
		"--runtime": true, "--platform": true, "--pull": true,
		"--ip": true, "--ip6": true, "--mac-address": true,
		"--expose": true, "--link": true, "--cidfile": true,
	}

	blockedFlags := map[string]bool{
		"--privileged":         true,
		"--cap-add":            true,
		"--cap-drop":           true,
		"--security-opt":       true,
		"--volumes-from":       true,
		"--env-file":           true,
		"--device":             true,
		"--device-cgroup-rule": true,
		"--sysctl":             true,
		"--oom-kill-disable":   true,
		"--oom-score-adj":      true,
	}

	namespaceFlagsBlockHost := map[string]bool{
		"--network":  true,
		"--net":      true,
		"--pid":      true,
		"--userns":   true,
		"--ipc":      true,
		"--uts":      true,
		"--cgroupns": true,
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if skipNext {
			skipNext = false
			continue
		}
		argLower := strings.ToLower(strings.TrimSpace(arg))
		flagName := argLower
		flagValue := ""
		if strings.HasPrefix(argLower, "--") {
			if k, v, ok := strings.Cut(argLower, "="); ok {
				flagName = k
				flagValue = strings.TrimSpace(v)
			}
		}

		if strings.HasPrefix(argLower, "-") {
			if blocked, match := matchesBlockedFlag(flagName, blockedFlags); blocked {
				fmt.Printf("[security] blocked dangerous docker flag: %s (matches %s)\n", flagName, match)
				return fmt.Errorf("blocked dangerous docker flag: %s", match)
			}
		}

		if matched, _ := matchesBlockedFlag(flagName, map[string]bool{"--mount": true}); matched || flagName == "--mount" {
			mountVal := flagValue
			if mountVal == "" && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				mountVal = strings.ToLower(strings.TrimSpace(args[i+1]))
				skipNext = true
			}
			mountVal = strings.ToLower(mountVal)
			if strings.Contains(mountVal, "type=bind") {
				fmt.Printf("[security] blocked --mount bind mount: %s\n", mountVal)
				return fmt.Errorf("blocked dangerous docker flag: --mount with bind")
			}
		}

		if matched, _ := matchesBlockedFlag(flagName, map[string]bool{"--tmpfs": true}); matched || flagName == "--tmpfs" {
			tmpfsVal := flagValue
			if tmpfsVal == "" && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				tmpfsVal = strings.ToLower(strings.TrimSpace(args[i+1]))
				skipNext = true
			}
			if strings.Contains(strings.ToLower(tmpfsVal), "exec") {
				fmt.Printf("[security] blocked --tmpfs with exec: %s\n", tmpfsVal)
				return fmt.Errorf("blocked dangerous docker flag: --tmpfs with exec")
			}
		}

		if blocked, _ := matchesBlockedFlag(flagName, namespaceFlagsBlockHost); blocked {
			nsMode := flagValue
			if nsMode == "" && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				nsMode = strings.ToLower(strings.TrimSpace(args[i+1]))
				skipNext = true
			}
			if nsMode == "host" {
				fmt.Printf("[security] blocked namespace escape: %s=host\n", flagName)
				return fmt.Errorf("blocked dangerous docker flag: %s=host", flagName)
			}
		}

		if strings.HasPrefix(argLower, "-") {
			if strings.Contains(argLower, "=") {
				continue
			}
			if flagsWithValue[flagName] {
				skipNext = true
			}
			continue
		}
		dockerImage = arg
		break
	}

	if dockerImage == "" {
		fmt.Printf("[docker] ERROR: could not find docker image in args: %v\n", args)
		return fmt.Errorf("could not find docker image in startup command")
	}

	if image != "" {
		normalizedConfigured := normalizeDockerImage(imageRef)
		normalizedCommand := normalizeDockerImage(dockerImage)
		if normalizedConfigured != normalizedCommand {
			fmt.Printf("[security] BLOCKED docker image mismatch at execution for %s: configured=%q command=%q\n", name, imageRef, dockerImage)
			return fmt.Errorf("docker image mismatch: the startup command image does not match the server's configured image")
		}
	}

	if !a.docker.imageExistsLocally(ctx, dockerImage) {
		fmt.Printf("[docker] pulling image: %s\n", dockerImage)
		a.docker.pull(ctx, dockerImage)
	} else {
		fmt.Printf("[docker] image already available locally, skipping pull: %s\n", dockerImage)
	}

	ensureVolumeWritable(serverDir)

	volumeContainerPaths := []string{}
	for i, arg := range args {
		if (arg == "-v" || arg == "--volume") && i+1 < len(args) {
			volSpec := args[i+1]
			parts := strings.Split(volSpec, ":")
			if len(parts) >= 2 {
				containerPath := parts[1]
				if idx := strings.Index(containerPath, ":"); idx > 0 {
					containerPath = containerPath[:idx]
				}
				volumeContainerPaths = append(volumeContainerPaths, containerPath)
			}
		} else if strings.HasPrefix(arg, "-v=") || strings.HasPrefix(arg, "--volume=") {
			volSpec := strings.SplitN(arg, "=", 2)[1]
			parts := strings.Split(volSpec, ":")
			if len(parts) >= 2 {
				containerPath := parts[1]
				if idx := strings.Index(containerPath, ":"); idx > 0 {
					containerPath = containerPath[:idx]
				}
				volumeContainerPaths = append(volumeContainerPaths, containerPath)
			}
		}
	}

	fmt.Printf("[docker] found volume container paths: %v\n", volumeContainerPaths)

	isGameServer := isGameServerDockerImage(dockerImage)

	shouldExtract := !isGameServer
	if shouldExtract {
		entries, err := os.ReadDir(serverDir)
		if err == nil {
			fileCount := 0
			for _, e := range entries {
				if e.Name() != ".adpanel" {
					fileCount++
				}
			}
			if fileCount > 0 {
				fmt.Printf("[docker] server directory already has %d files, skipping extraction to preserve user changes\n", fileCount)
				shouldExtract = false
			}
		}
	} else {
		fmt.Printf("[docker] game server image detected (%s), skipping file extraction — container will download files\n", dockerImage)
	}

	extracted := false
	if shouldExtract {
		for _, containerPath := range volumeContainerPaths {
			err := a.docker.extractImageFiles(ctx, dockerImage, serverDir, containerPath)
			if err == nil {
				fmt.Printf("[docker] extracted files from %s:%s to server directory\n", dockerImage, containerPath)
				extracted = true
				break
			}
		}

		if !extracted {
			imgCfg := a.docker.inspectImageConfig(ctx, dockerImage)
			for _, p := range imgCfg.Volumes {
				err := a.docker.extractImageFiles(ctx, dockerImage, serverDir, p)
				if err == nil {
					fmt.Printf("[docker] extracted files from %s:%s (image VOLUME) to server directory\n", dockerImage, p)
					extracted = true
					break
				}
			}
			if !extracted {
				for _, ep := range getDataPathsFromEnv(imgCfg.Env) {
					err := a.docker.extractImageFiles(ctx, dockerImage, serverDir, ep)
					if err == nil {
						fmt.Printf("[docker] extracted files from %s:%s (image ENV) to server directory\n", dockerImage, ep)
						extracted = true
						break
					}
				}
			}
			if !extracted && imgCfg.Workdir != "" && imgCfg.Workdir != "/" {
				err := a.docker.extractImageFiles(ctx, dockerImage, serverDir, imgCfg.Workdir)
				if err == nil {
					fmt.Printf("[docker] extracted files from %s:%s (image WORKDIR) to server directory\n", dockerImage, imgCfg.Workdir)
					extracted = true
				}
			}
		}

		if !extracted {
			commonPaths := []string{"/app", "/data", "/config", "/srv"}
			for _, extractPath := range commonPaths {
				err := a.docker.extractImageFiles(ctx, dockerImage, serverDir, extractPath)
				if err == nil {
					fmt.Printf("[docker] extracted files from %s:%s to server directory\n", dockerImage, extractPath)
					extracted = true
					break
				}
			}
		}

		if !extracted {
			fmt.Printf("[docker] note: could not extract files from image (image may use runtime-only setup)\n")
		}
	}

	firstContainerPort := 0
	hostToContainerPort := make(map[int]int)
	filteredArgs := []string{}
	skipNext = false
	skipNextVolume := false
	skipNextRestart := false
	skipNextPort := false
	for i, arg := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if skipNextRestart {
			skipNextRestart = false
			continue
		}
		if skipNextPort {
			skipNextPort = false
			cp := extractContainerPortFromMapping(arg)
			hp := extractHostPortFromMapping(arg)
			if firstContainerPort == 0 && cp > 0 {
				firstContainerPort = cp
			}
			if hp > 0 && cp > 0 {
				hostToContainerPort[hp] = cp
			}
			continue
		}
		if skipNextVolume {
			skipNextVolume = false
			parts := strings.Split(arg, ":")
			if len(parts) >= 2 {
				containerPath := parts[1]
				if idx := strings.LastIndex(containerPath, ":"); idx > 0 {
					suffix := containerPath[idx:]
					containerPath = containerPath[:idx]
					filteredArgs = append(filteredArgs, fmt.Sprintf("%s:%s%s", serverDir, containerPath, suffix))
				} else {
					filteredArgs = append(filteredArgs, fmt.Sprintf("%s:%s", serverDir, containerPath))
				}
			} else {
				filteredArgs = append(filteredArgs, arg)
			}
			continue
		}
		if arg == "-d" || arg == "--detach" {
			continue
		}
		if (arg == "-p" || arg == "--publish") && i+1 < len(args) {
			skipNextPort = true
			continue
		}
		if strings.HasPrefix(arg, "-p=") || strings.HasPrefix(arg, "--publish=") {
			portVal := ""
			if strings.HasPrefix(arg, "-p=") {
				portVal = strings.TrimPrefix(arg, "-p=")
			} else {
				portVal = strings.TrimPrefix(arg, "--publish=")
			}
			cp := extractContainerPortFromMapping(portVal)
			hp := extractHostPortFromMapping(portVal)
			if firstContainerPort == 0 && cp > 0 {
				firstContainerPort = cp
			}
			if hp > 0 && cp > 0 {
				hostToContainerPort[hp] = cp
			}
			continue
		}
		if arg == "--restart" && i+1 < len(args) {
			skipNextRestart = true
			continue
		}
		if strings.HasPrefix(arg, "--restart=") {
			continue
		}
		if arg == "--name" && i+1 < len(args) {
			skipNext = true
			continue
		}
		if strings.HasPrefix(arg, "--name=") {
			continue
		}
		if (arg == "-m" || arg == "--memory" || arg == "--memory-swap" || arg == "--cpu-period" || arg == "--cpu-quota") && i+1 < len(args) {
			skipNext = true
			continue
		}
		if strings.HasPrefix(arg, "-m=") || strings.HasPrefix(arg, "--memory=") || strings.HasPrefix(arg, "--memory-swap=") || strings.HasPrefix(arg, "--cpu-period=") || strings.HasPrefix(arg, "--cpu-quota=") {
			continue
		}
		if arg == "--cpus" && i+1 < len(args) {
			skipNext = true
			continue
		}
		if strings.HasPrefix(arg, "--cpus=") {
			continue
		}
		if (arg == "-v" || arg == "--volume") && i+1 < len(args) {
			filteredArgs = append(filteredArgs, arg)
			skipNextVolume = true
			continue
		}
		if strings.HasPrefix(arg, "-v=") || strings.HasPrefix(arg, "--volume=") {
			prefix := "-v="
			if strings.HasPrefix(arg, "--volume=") {
				prefix = "--volume="
			}
			volSpec := strings.TrimPrefix(arg, prefix)
			parts := strings.Split(volSpec, ":")
			if len(parts) >= 2 {
				containerPath := parts[1]
				if idx := strings.LastIndex(containerPath, ":"); idx > 0 {
					suffix := containerPath[idx:]
					containerPath = containerPath[:idx]
					filteredArgs = append(filteredArgs, fmt.Sprintf("%s%s:%s%s", prefix, serverDir, containerPath, suffix))
				} else {
					filteredArgs = append(filteredArgs, fmt.Sprintf("%s%s:%s", prefix, serverDir, containerPath))
				}
			} else {
				filteredArgs = append(filteredArgs, arg)
			}
			continue
		}
		filteredArgs = append(filteredArgs, arg)
	}

	pidsLimit := defaultPidsLimit
	if resources != nil && resources.PidsLimit != nil && *resources.PidsLimit >= minPidsLimit && *resources.PidsLimit <= maxPidsLimit {
		pidsLimit = *resources.PidsLimit
	}
	finalArgs := dockerRunInteractiveArgs()
	finalArgs = append(finalArgs, "--restart", "unless-stopped", "--name", name)
	finalArgs = append(finalArgs, "--cap-drop=ALL", fmt.Sprintf("--pids-limit=%d", pidsLimit))
	finalArgs = append(finalArgs, panelSafeCaps()...)
	if resources != nil {
		if resources.RamMb != nil && *resources.RamMb > 0 {
			finalArgs = append(finalArgs, "--memory", fmt.Sprintf("%dm", *resources.RamMb))
			finalArgs = append(finalArgs, "--memory-reservation", fmt.Sprintf("%dm", *resources.RamMb*90/100))
			if resources.SwapMb == nil {
				finalArgs = append(finalArgs, "--memory-swap", fmt.Sprintf("%dm", *resources.RamMb))
			} else {
				swapVal := *resources.SwapMb
				if swapVal == -1 {
					finalArgs = append(finalArgs, "--memory-swap", fmt.Sprintf("%dm", *resources.RamMb))
				} else if swapVal == 0 {
					finalArgs = append(finalArgs, "--memory-swap", "-1")
				} else {
					totalSwap := *resources.RamMb + swapVal
					finalArgs = append(finalArgs, "--memory-swap", fmt.Sprintf("%dm", totalSwap))
				}
			}
		}
		if resources.CpuCores != nil && *resources.CpuCores > 0 && *resources.CpuCores <= 128 {
			effCores, _ := computeCpuLimit(*resources.CpuCores)
			cpuQuota := int64(float64(cpuPeriod) * effCores)
			finalArgs = append(finalArgs, "--cpu-period", fmt.Sprintf("%d", cpuPeriod))
			finalArgs = append(finalArgs, "--cpu-quota", fmt.Sprintf("%d", cpuQuota))
		}
		if resources.CpuWeight != nil && *resources.CpuWeight >= minCpuWeight && *resources.CpuWeight <= maxCpuWeight {
			finalArgs = append(finalArgs, "--cpu-shares", fmt.Sprintf("%d", dockerCpuSharesFromWeight(*resources.CpuWeight)))
		}
		if resources.IoWeight != nil && *resources.IoWeight >= minIoWeight && *resources.IoWeight <= maxIoWeight {
			finalArgs = append(finalArgs, "--blkio-weight", fmt.Sprintf("%d", *resources.IoWeight))
		}
		if resources.FileLimit != nil && *resources.FileLimit >= minFileLimit && *resources.FileLimit <= maxFileLimit {
			finalArgs = append(finalArgs, "--ulimit", fmt.Sprintf("nofile=%d:%d", *resources.FileLimit, *resources.FileLimit))
		}
	}

	effContainerPort := 0
	if hostPort > 0 {
		effContainerPort = hostPort
		if firstContainerPort > 0 {
			effContainerPort = firstContainerPort
		}
		finalArgs = append(finalArgs, "-p", fmt.Sprintf("%d:%d", hostPort, effContainerPort))
		fmt.Printf("[port-enforce] %s: only exposing allocated port %d -> container %d (stripped all other -p flags)\n", name, hostPort, effContainerPort)
	} else if firstContainerPort > 0 {
		effContainerPort = firstContainerPort
	}
	if metaRes, ok := meta["resources"].(map[string]any); ok {
		for _, ap := range runtimeIntSlice(metaRes["ports"]) {
			if ap > 0 && ap != hostPort {
				finalArgs = append(finalArgs, "-p", fmt.Sprintf("%d:%d", ap, ap))
				fmt.Printf("[port-manage] %s: exposing additional port %d:%d\n", name, ap, ap)
			}
		}
	}
	portForwards := a.loadPortForwards(serverDir)
	for _, pf := range portForwards {
		targetContainerPort := pf.Internal
		if cp, ok := hostToContainerPort[pf.Internal]; ok {
			targetContainerPort = cp
		} else if effContainerPort > 0 {
			if pf.Internal == hostPort {
				targetContainerPort = effContainerPort
			}
		}
		finalArgs = append(finalArgs, "-p", fmt.Sprintf("%d:%d", pf.Public, targetContainerPort))
		fmt.Printf("[port-forwards] %s: forwarding public %d -> container %d (internal host port was %d)\n", name, pf.Public, targetContainerPort, pf.Internal)
	}

	finalArgs = append(finalArgs, filteredArgs...)

	hasVolumeMount := false
	for _, arg := range filteredArgs {
		if arg == "-v" || arg == "--volume" || strings.HasPrefix(arg, "-v=") || strings.HasPrefix(arg, "--volume=") {
			hasVolumeMount = true
			break
		}
	}
	if !hasVolumeMount {
		fmt.Printf("[docker] WARNING: no volume mounts in startup command for %s — auto-injecting from image inspection\n", name)
		imgCfg := a.docker.inspectImageConfig(ctx, dockerImage)
		injected := false
		for _, volPath := range imgCfg.Volumes {
			finalArgs = append(finalArgs, "-v", fmt.Sprintf("%s:%s", serverDir, volPath))
			fmt.Printf("[docker] auto-injected volume: %s -> %s (from image VOLUME)\n", serverDir, volPath)
			injected = true
		}
		if !injected {
			envPaths := getDataPathsFromEnv(imgCfg.Env)
			for _, envPath := range envPaths {
				finalArgs = append(finalArgs, "-v", fmt.Sprintf("%s:%s", serverDir, envPath))
				fmt.Printf("[docker] auto-injected volume: %s -> %s (from image ENV)\n", serverDir, envPath)
				injected = true
				break
			}
		}
		if !injected && imgCfg.Workdir != "" && imgCfg.Workdir != "/" {
			finalArgs = append(finalArgs, "-v", fmt.Sprintf("%s:%s", serverDir, imgCfg.Workdir))
			fmt.Printf("[docker] auto-injected volume: %s -> %s (from image WORKDIR)\n", serverDir, imgCfg.Workdir)
			injected = true
		}
		if !injected {
			defaultPath := "/data"
			if !isGameServerDockerImage(dockerImage) {
				defaultPath = "/app"
			}
			finalArgs = append(finalArgs, "-v", fmt.Sprintf("%s:%s", serverDir, defaultPath))
			fmt.Printf("[docker] auto-injected volume: %s -> %s (default fallback)\n", serverDir, defaultPath)
		}
	}

	if a.debug {
		fmt.Printf("[docker] executing: docker %v\n", finalArgs)
	}

	outStr, errStr, exitCode, err := a.docker.runCollect(ctx, finalArgs...)
	if err != nil || exitCode != 0 {
		fmt.Printf("[docker] startup command error (exit %d): stdout=%s stderr=%s\n", exitCode, strings.TrimSpace(outStr), strings.TrimSpace(errStr))
		return fmt.Errorf("container start failed: %s", strings.TrimSpace(errStr))
	}

	containerId := strings.TrimSpace(outStr)
	if containerId != "" {
		fmt.Printf("[docker] container started with ID: %s\n", containerId[:12])
	}

	go a.recoverMisplacedFiles(name, serverDir)

	time.Sleep(500 * time.Millisecond)
	statusOut, _, _, _ := a.docker.runCollect(ctx, "inspect", "--format", "{{.State.Status}} {{.State.ExitCode}}", name)
	statusParts := strings.Fields(strings.TrimSpace(statusOut))
	if len(statusParts) >= 1 {
		status := statusParts[0]
		exitStr := "0"
		if len(statusParts) >= 2 {
			exitStr = statusParts[1]
		}
		fmt.Printf("[docker] container status after start: %s (exit code: %s)\n", status, exitStr)

		if status == "exited" || status == "dead" {
			logsOut, _, _, _ := a.docker.runCollect(ctx, "logs", "--tail", "50", name)
			fmt.Printf("[docker] container crashed! Last logs:\n%s\n", logsOut)
			return fmt.Errorf("container exited immediately with code %s", exitStr)
		}
	}

	return nil
}

func extractContainerPortFromMapping(mapping string) int {
	s := mapping
	if slashIdx := strings.Index(s, "/"); slashIdx > 0 {
		s = s[:slashIdx]
	}
	parts := strings.Split(s, ":")
	if len(parts) == 0 {
		return 0
	}
	last := strings.TrimSpace(parts[len(parts)-1])
	p, _ := strconv.Atoi(last)
	return p
}

func extractHostPortFromMapping(mapping string) int {
	s := mapping
	if slashIdx := strings.Index(s, "/"); slashIdx > 0 {
		s = s[:slashIdx]
	}
	parts := strings.Split(s, ":")
	switch len(parts) {
	case 1:
		return 0
	case 2:
		p, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
		return p
	case 3:
		p, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
		return p
	}
	return 0
}

func validateStartupCommandPorts(startupCmd string, allocatedPort int, additionalAllocatedPorts []int, reservedPorts ...int) string {
	allowedPorts := make([]int, 0, 1+len(additionalAllocatedPorts))
	if allocatedPort > 0 {
		allowedPorts = append(allowedPorts, allocatedPort)
	}
	for _, p := range additionalAllocatedPorts {
		if p > 0 {
			allowedPorts = append(allowedPorts, p)
		}
	}
	return validateDockerRunCommandSafety(startupCmd, allowedPorts, reservedPorts, "")
}

func extractDockerImageFromCommand(cmdStr string) string {
	cmd := strings.TrimSpace(cmdStr)
	cmdLower := strings.ToLower(cmd)
	if !strings.HasPrefix(cmdLower, "docker run") {
		return ""
	}
	argsStr := strings.TrimSpace(cmd[len("docker run"):])
	args := parseShellArgs(argsStr)

	flagsWithValue := map[string]bool{
		"-e": true, "--env": true, "-v": true, "--volume": true,
		"-p": true, "--publish": true, "-w": true, "--workdir": true,
		"--name": true, "-m": true, "--memory": true, "--cpus": true,
		"--memory-swap": true, "--memory-reservation": true, "--cpu-period": true, "--cpu-quota": true,
		"-u": true, "--user": true, "-h": true, "--hostname": true,
		"--network": true, "--net": true, "--restart": true,
		"-l": true, "--label": true, "--entrypoint": true,
		"--mount": true, "--tmpfs": true, "--device": true,
		"--pid": true, "--ipc": true, "--userns": true, "--uts": true, "--cgroupns": true,
		"--shm-size": true, "--ulimit": true, "--dns": true, "--dns-search": true,
		"--add-host": true, "--log-driver": true, "--log-opt": true,
		"--stop-signal": true, "--stop-timeout": true, "--health-cmd": true,
		"--health-interval": true, "--health-retries": true, "--health-timeout": true,
		"--runtime": true, "--platform": true, "--pull": true,
		"--ip": true, "--ip6": true, "--mac-address": true,
		"--expose": true, "--link": true, "--cidfile": true,
	}

	skipNext := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if skipNext {
			skipNext = false
			continue
		}
		argLower := strings.ToLower(strings.TrimSpace(arg))
		flagName := argLower
		if strings.HasPrefix(argLower, "--") {
			if k, _, ok := strings.Cut(argLower, "="); ok {
				flagName = k
			}
		}
		if strings.HasPrefix(argLower, "-") {
			if strings.Contains(argLower, "=") {
				continue
			}
			if flagsWithValue[flagName] {
				skipNext = true
			}
			continue
		}
		return arg
	}
	return ""
}

func normalizeDockerImage(raw string) string {
	img := strings.TrimSpace(raw)
	if img == "" {
		return ""
	}
	if at := strings.Index(img, "@"); at > 0 {
		img = img[:at]
	}
	if colon := strings.LastIndex(img, ":"); colon > 0 {
		afterColon := img[colon+1:]
		if !strings.Contains(afterColon, "/") {
			img = img[:colon]
		}
	}
	img = strings.ToLower(img)
	img = strings.TrimPrefix(img, "docker.io/")
	img = strings.TrimPrefix(img, "index.docker.io/")
	if !strings.Contains(img, "/") {
		img = "library/" + img
	}
	return img
}

func parseShellArgs(s string) []string {
	var args []string
	var current strings.Builder
	inQuote := rune(0)
	escape := false

	for _, r := range s {
		if escape {
			if r == '\n' || r == '\r' {
				escape = false
				continue
			}
			current.WriteRune(r)
			escape = false
			continue
		}
		if r == '\\' {
			escape = true
			continue
		}
		if inQuote != 0 {
			if r == inQuote {
				inQuote = 0
			} else {
				current.WriteRune(r)
			}
			continue
		}
		if r == '"' || r == '\'' {
			inQuote = r
			continue
		}
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if current.Len() > 0 {
				args = append(args, current.String())
				current.Reset()
			}
			continue
		}
		current.WriteRune(r)
	}
	if current.Len() > 0 {
		args = append(args, current.String())
	}
	return args
}

func (a *Agent) startCustomDockerContainer(ctx context.Context, name, serverDir string, meta map[string]any, hostPort int, resources *resourceLimits) error {
	a.docker.rmForce(ctx, name)

	runtimeObj, _ := meta["runtime"].(map[string]any)
	if runtimeObj == nil {
		runtimeObj = make(map[string]any)
	}
	if a.debug {
		fmt.Printf("[docker] startCustomDockerContainer for %s\n", name)
	}

	image := strings.TrimSpace(fmt.Sprint(runtimeObj["image"]))
	tag := strings.TrimSpace(fmt.Sprint(runtimeObj["tag"]))
	if image == "" || image == "<nil>" {
		return fmt.Errorf("no docker image specified")
	}
	if tag == "" || tag == "<nil>" {
		tag = "latest"
	}
	imageRef := image + ":" + tag
	imgCfg := a.inspectRuntimeImageConfig(ctx, imageRef)

	ensureVolumeWritable(serverDir)
	tpl := strings.ToLower(fmt.Sprint(meta["type"]))
	if tpl == "" || tpl == "<nil>" {
		tpl = strings.ToLower(fmt.Sprint(runtimeObj["providerId"]))
	}
	if normalizeCfxRuntimeLayout(tpl, runtimeObj, serverDir) {
		meta["runtime"] = runtimeObj
		_ = a.saveMeta(serverDir, meta)
	}

	hasCustomVolumes := len(runtimeStringSlice(runtimeObj["volumes"])) > 0
	explicitCommandTemplate := sanitizeRuntimeCommand(runtimeObj["command"])
	adpanelStartup := startupTemplateFromRuntime(runtimeObj)
	installCfg := installConfigFromRuntime(runtimeObj)
	runtimeInstallPath := ""
	if installCfg != nil {
		runtimeInstallPath = normalizeContainerWorkdir(installCfg.RuntimePath)
	}
	if runtimeInstallPath == "" && adpanelStartup != "" {
		runtimeInstallPath = "/home/container"
	}

	binds, dataDir := a.resolveStructuredBinds(tpl, runtimeObj, serverDir)
	workdir := normalizeContainerWorkdir(runtimeObj["workdir"])
	if !hasCustomVolumes {
		inferredDataPath := false
		if runtimeInstallPath != "" {
			dataDir = runtimeInstallPath
			binds = []string{fmt.Sprintf("%s:%s", serverDir, dataDir)}
			inferredDataPath = true
			if workdir == "" {
				workdir = runtimeInstallPath
			}
		} else if len(imgCfg.Volumes) > 0 {
			dataDir = imgCfg.Volumes[0]
			binds = []string{fmt.Sprintf("%s:%s", serverDir, dataDir)}
			inferredDataPath = true
		} else if envPaths := getDataPathsFromEnv(imgCfg.Env); len(envPaths) > 0 {
			dataDir = envPaths[0]
			binds = []string{fmt.Sprintf("%s:%s", serverDir, dataDir)}
			inferredDataPath = true
		} else if imgCfg.Workdir != "" && imgCfg.Workdir != "/" && (explicitCommandTemplate != "" || hasManagedRuntimeDefaults(tpl)) {
			dataDir = imgCfg.Workdir
			binds = []string{fmt.Sprintf("%s:%s", serverDir, dataDir)}
			inferredDataPath = true
			if workdir == "" {
				workdir = imgCfg.Workdir
			}
		}
		if !inferredDataPath && explicitCommandTemplate == "" && !hasManagedRuntimeDefaults(tpl) {
			dataDir = ""
			binds = nil
		}
	} else if explicitCommandTemplate == "" || shouldPreferImageDefaultRuntimeCommand(explicitCommandTemplate, imgCfg) {
		binds, dataDir = sanitizeCustomImageDefaultBinds(imageRef, imgCfg, binds, dataDir)
	}

	preparePaths := []string{dataDir, workdir, imgCfg.Workdir}
	for _, bind := range binds {
		preparePaths = append(preparePaths, parseVolumeSpecContainerPath(bind))
	}
	if explicitCommandTemplate != "" {
		preparePaths = append(preparePaths, commandReferencedContainerPaths(explicitCommandTemplate, workdir, dataDir)...)
	}
	preparedPath := a.prepareServerDirFromImage(ctx, imageRef, serverDir, imgCfg, preparePaths)
	if preparedPath != "" && len(binds) == 0 && !hasManagedRuntimeDefaults(tpl) {
		dataDir = preparedPath
		binds = []string{fmt.Sprintf("%s:%s", serverDir, dataDir)}
		if workdir == "" {
			workdir = dataDir
		}
		fmt.Printf("[docker] mounted prepared image path for %s: %s -> %s\n", imageRef, serverDir, dataDir)
	}

	env := runtimeEnvMap(runtimeObj["env"])
	if env == nil {
		env = map[string]string{}
	}
	applyRuntimeHostPortEnv(tpl, env, hostPort)
	renderRuntimeEnvTemplates(tpl, env, hostPort)
	if adpanelStartup != "" {
		for key, value := range adpanelStartupDefaultEnv(name, hostPort, resources, adpanelStartup) {
			if _, exists := env[key]; !exists {
				env[key] = value
			}
		}
		env["STARTUP"] = renderADPanelStartupTemplate(adpanelStartup, env, hostPort)
	}

	mainContainerPort := hostPort
	if ports := runtimeIntSlice(runtimeObj["ports"]); len(ports) > 0 {
		if p := ports[0]; p > 0 {
			mainContainerPort = p
		}
	}
	if runtimeUsesHostPortInsideContainer(tpl, runtimeObj, env) && hostPort > 0 {
		mainContainerPort = hostPort
	}
	if mainContainerPort <= 0 {
		mainContainerPort = hostPort
	}

	startFile := strings.TrimSpace(fmt.Sprint(meta["start"]))
	if startFile == "" || startFile == "<nil>" {
		startFile = defaultStartFileForType(tpl)
	}
	commandTemplate := explicitCommandTemplate
	if adpanelStartup != "" {
		commandTemplate = ""
	} else if commandTemplate == "" {
		commandTemplate = defaultRuntimeCommandForType(tpl, startFile, dataDir)
	}
	if commandTemplate == "" && !imageConfigHasRuntimeDefault(imgCfg) {
		if adpanelStartup != "" && strings.TrimSpace(env["STARTUP"]) != "" {
			commandTemplate = ""
		} else {
			commandTemplate = "sleep infinity"
		}
	}
	if commandTemplate != "" && shouldPreferImageDefaultRuntimeCommand(commandTemplate, imgCfg) {
		fmt.Printf("[docker] ignoring path-only template command for %s and using image default entrypoint/cmd: %s\n", imageRef, commandTemplate)
		commandTemplate = ""
		if imageWorkdir := normalizeContainerWorkdir(imgCfg.Workdir); imageWorkdir != "" {
			workdir = imageWorkdir
		}
	}
	var cmdArgs []string
	if adpanelStartup != "" && !imageConfigHasRuntimeDefault(imgCfg) && strings.TrimSpace(env["STARTUP"]) != "" {
		cmdArgs = []string{"sh", "-lc", env["STARTUP"]}
	} else if commandTemplate != "" {
		ensureRuntimeEntrypointScaffold(serverDir, tpl, commandTemplate, dataDir, workdir, startFile, hostPort)
		var err error
		cmdArgs, err = buildRenderedRuntimeProcessArgs(tpl, commandTemplate, dataDir, startFile, imageRef, hostPort, resources, meta)
		if err != nil {
			return err
		}
	}

	uid, gid := statUIDGID(serverDir)
	req := dockerContainerCreateRequest{
		Image:        imageRef,
		Env:          envMapToList(env),
		Cmd:          cmdArgs,
		WorkingDir:   workdir,
		User:         a.structuredContainerUser(ctx, imageRef, uid, gid, runtimeObj),
		Tty:          containerTTYForHost(),
		OpenStdin:    true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	}
	portBindings, exposedPorts := a.buildStructuredPortBindings(serverDir, meta, hostPort, mainContainerPort)
	req.ExposedPorts = exposedPorts
	desiredRestart := strings.TrimSpace(fmt.Sprint(runtimeObj["restart"]))
	if desiredRestart == "<nil>" {
		desiredRestart = ""
	}
	resources = withRuntimeDefaultFileLimit(resources, tpl, imageRef)
	req.HostConfig = buildStructuredHostConfig(
		binds,
		portBindings,
		resources,
		"no",
		false,
	)

	if err := a.runStructuredContainerWithCompatUser(ctx, name, req); err != nil {
		return err
	}
	if err := a.monitorCustomContainerBootstrap(ctx, name, imageRef); err != nil {
		return err
	}
	if desiredRestart != "" && strings.ToLower(desiredRestart) != "no" {
		a.docker.updateRestartPolicy(ctx, name, desiredRestart)
	}
	go a.recoverMisplacedFiles(name, serverDir)
	return nil
}

func isGameServerDockerImage(image string) bool {
	imageLower := strings.ToLower(image)
	gameServerPatterns := []string{
		"csgo", "cs2", "counterstrike",
		"minecraft", "paper", "spigot", "bukkit", "forge", "fabric",
		"hytale", "hytale-docker", "rxmarin/hytale",
		"valheim", "rust", "ark", "terraria",
		"assetto", "dayz", "zomboid",
		"fivem", "redm", "ragemp", "gtav", "gta5",
		"factorio", "satisfactory",
		"teamspeak", "ts3", "mumble",
		"starbound", "unturned", "7daystodie", "7dtd",
		"conan", "empyrion", "hurtworld",
		"mordhau", "squad", "insurgency",
		"garrysmod", "gmod", "srcds",
		"avorion", "astroneer", "barotrauma",
		"palworld", "enshrouded", "soulmask",
		"ptero-eggs",
		"cm2network", "gameservermanagers", "linuxgsm",
		"steamcmd", "itzg/minecraft", "ich777",
	}
	for _, pattern := range gameServerPatterns {
		if strings.Contains(imageLower, pattern) {
			return true
		}
	}
	return false
}

func getGameServerDataPath(image string) string {
	imageLower := strings.ToLower(image)

	if strings.Contains(imageLower, "cm2network/csgo") || strings.Contains(imageLower, "csgo") {
		return "/home/steam/csgo-dedicated"
	}
	if strings.Contains(imageLower, "cm2network/cs2") || strings.Contains(imageLower, "cs2") {
		return "/home/steam/cs2-dedicated"
	}
	if strings.Contains(imageLower, "cm2network/tf2") {
		return "/home/steam/tf2-dedicated"
	}

	if strings.Contains(imageLower, "fivem") || strings.Contains(imageLower, "cfx") {
		return "/config"
	}

	if strings.Contains(imageLower, "ragemp") {
		return "/ragemp"
	}

	if strings.Contains(imageLower, "valheim") {
		return "/config"
	}

	if strings.Contains(imageLower, "rust") && !strings.Contains(imageLower, "rustlang") {
		return "/steamcmd/rust"
	}

	if strings.Contains(imageLower, "ark") {
		return "/ark"
	}

	if strings.Contains(imageLower, "itzg/minecraft") {
		return "/data"
	}

	if strings.Contains(imageLower, "terraria") {
		return "/config"
	}

	if strings.Contains(imageLower, "factorio") {
		return "/factorio"
	}

	if strings.Contains(imageLower, "satisfactory") {
		return "/config"
	}

	if strings.Contains(imageLower, "teamspeak") || strings.Contains(imageLower, "ts3") {
		return "/var/ts3server"
	}

	if strings.Contains(imageLower, "7daystodie") || strings.Contains(imageLower, "7dtd") {
		return "/steamcmd/7dtd"
	}

	if strings.Contains(imageLower, "garrysmod") || strings.Contains(imageLower, "gmod") {
		return "/home/steam/gmod-dedicated"
	}

	if strings.Contains(imageLower, "palworld") {
		return "/palworld"
	}

	if strings.Contains(imageLower, "enshrouded") {
		return "/config"
	}

	if strings.Contains(imageLower, "linuxgsm") || strings.Contains(imageLower, "gameservermanagers") {
		return "/home/linuxgsm"
	}

	if strings.Contains(imageLower, "ich777") {
		return "/serverdata"
	}

	return "/data"
}

func getGameServerUID(image string) int {
	imageLower := strings.ToLower(image)

	if strings.Contains(imageLower, "cm2network") {
		return 1000
	}

	if strings.Contains(imageLower, "itzg/minecraft") {
		return 1000
	}

	if strings.Contains(imageLower, "linuxgsm") || strings.Contains(imageLower, "gameservermanagers") {
		return 1000
	}

	if strings.Contains(imageLower, "ich777") {
		return 99
	}

	return 1000
}

func fixGameServerVolumePermissions(serverDir string, image string) error {
	uid := getGameServerUID(image)
	gid := uid

	fmt.Printf("[docker] Setting volume permissions for game server: chown -R %d:%d %s\n", uid, gid, serverDir)

	resolvedServerDir, resolveErr := filepath.EvalSymlinks(serverDir)
	if resolveErr != nil {
		return fmt.Errorf("cannot resolve server directory path: %v", resolveErr)
	}
	if resolvedServerDir != filepath.Clean(serverDir) {
		fmt.Printf("[security] serverDir is a symlink: %s -> %s, using resolved path\n", serverDir, resolvedServerDir)
		serverDir = resolvedServerDir
	}

	cmd := exec.Command("chown", "-Rh", fmt.Sprintf("%d:%d", uid, gid), serverDir)
	if output, err := cmd.CombinedOutput(); err != nil {
		fmt.Printf("[docker] Warning: chown failed: %v, output: %s\n", err, string(output))
		if err2 := os.Chown(serverDir, uid, gid); err2 != nil {
			return fmt.Errorf("failed to set volume permissions: %v", err2)
		}
	}

	if err := os.Chmod(serverDir, 0755); err != nil {
		fmt.Printf("[docker] Warning: failed to chmod %s: %v\n", serverDir, err)
	}

	fmt.Printf("[docker] Volume permissions set successfully for %s\n", serverDir)
	return nil
}

func guessStartupCommand(serverDir string, image string) string {
	imageLower := strings.ToLower(image)

	if _, err := os.Stat(filepath.Join(serverDir, "package.json")); err == nil {
		return "cd /app && npm install --production && npm start"
	}

	if _, err := os.Stat(filepath.Join(serverDir, "requirements.txt")); err == nil {
		if _, err := os.Stat(filepath.Join(serverDir, "main.py")); err == nil {
			return "cd /app && pip install -r requirements.txt && python main.py"
		}
		if _, err := os.Stat(filepath.Join(serverDir, "app.py")); err == nil {
			return "cd /app && pip install -r requirements.txt && python app.py"
		}
		if _, err := os.Stat(filepath.Join(serverDir, "run.py")); err == nil {
			return "cd /app && pip install -r requirements.txt && python run.py"
		}
		return "cd /app && pip install -r requirements.txt && python main.py"
	}

	if strings.Contains(imageLower, "python") {
		if _, err := os.Stat(filepath.Join(serverDir, "main.py")); err == nil {
			return "python /app/main.py"
		}
		if _, err := os.Stat(filepath.Join(serverDir, "app.py")); err == nil {
			return "python /app/app.py"
		}
		return "python /app/main.py"
	}

	if strings.Contains(imageLower, "node") {
		if _, err := os.Stat(filepath.Join(serverDir, "index.js")); err == nil {
			return "node /app/index.js"
		}
		if _, err := os.Stat(filepath.Join(serverDir, "app.js")); err == nil {
			return "node /app/app.js"
		}
		return "node /app/index.js"
	}

	if strings.Contains(imageLower, "go") || strings.Contains(imageLower, "golang") {
		if _, err := os.Stat(filepath.Join(serverDir, "main.go")); err == nil {
			return "cd /app && go run main.go"
		}
		return "cd /app && go run ."
	}

	if strings.Contains(imageLower, "java") || strings.Contains(imageLower, "openjdk") {
		files, _ := filepath.Glob(filepath.Join(serverDir, "*.jar"))
		if len(files) > 0 {
			jarName := filepath.Base(files[0])
			return fmt.Sprintf("java -jar /app/%s", jarName)
		}
		return "java -jar /app/app.jar"
	}

	if strings.Contains(imageLower, "ruby") {
		if _, err := os.Stat(filepath.Join(serverDir, "Gemfile")); err == nil {
			return "cd /app && bundle install && bundle exec ruby main.rb"
		}
		return "ruby /app/main.rb"
	}

	if strings.Contains(imageLower, "php") {
		return "php -S 0.0.0.0:${PORT:-8080} -t /app"
	}

	if strings.Contains(imageLower, "nginx") {
		return ""
	}

	if strings.Contains(imageLower, "redis") {
		return ""
	}

	if strings.Contains(imageLower, "postgres") || strings.Contains(imageLower, "mysql") || strings.Contains(imageLower, "mariadb") {
		return ""
	}

	return ""
}

type logTail struct {
	name                 string
	closed               bool
	dockerSnapshotPrimed bool

	mu      sync.Mutex
	writeMu sync.Mutex
	clients map[chan string]struct{}
	recent  []string
	recentN int

	stopCh chan struct{}
	cmd    *exec.Cmd
	conn   io.ReadWriteCloser
}

type logSnapshotCall struct {
	done    chan struct{}
	entries []logSnapshotEntry
}

type logSnapshotEntry struct {
	line string
	ts   int64
}

const logTailRecentMaxLines = 1200

type logManager struct {
	mu    sync.Mutex
	tails map[string]*logTail
	d     Docker
	debug bool

	snapshotMu       sync.Mutex
	snapshotInFlight map[string]*logSnapshotCall
}

func newLogManager(d Docker, debug bool) *logManager {
	return &logManager{
		tails:            map[string]*logTail{},
		d:                d,
		debug:            debug,
		snapshotInFlight: map[string]*logSnapshotCall{},
	}
}

func normalizeStreamedLogLine(raw string) string {
	line := strings.TrimRight(raw, "\r\n")
	if strings.HasPrefix(line, "stdout:") {
		line = strings.TrimPrefix(line, "stdout:")
	} else if strings.HasPrefix(line, "stderr:") {
		line = strings.TrimPrefix(line, "stderr:")
	}
	return cleanLogLine(line)
}

func (t *logTail) appendRecentLocked(line string) {
	if line == "" {
		return
	}
	limit := t.recentN
	if limit <= 0 {
		limit = logTailRecentMaxLines
	}
	for _, part := range strings.Split(line, "\n") {
		part = strings.TrimRight(part, "\r")
		if part == "" {
			continue
		}
		t.recent = append(t.recent, part)
	}
	if overflow := len(t.recent) - limit; overflow > 0 {
		copy(t.recent, t.recent[overflow:])
		t.recent = t.recent[:limit]
	}
}

func (t *logTail) appendRecentBatch(lines []string) {
	if len(lines) == 0 {
		return
	}
	t.mu.Lock()
	for _, line := range lines {
		t.appendRecentLocked(cleanLogLine(line))
	}
	t.mu.Unlock()
}

func (t *logTail) snapshotRecent(tail int) []string {
	if tail <= 0 {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.recent) == 0 {
		return nil
	}
	if tail > len(t.recent) {
		tail = len(t.recent)
	}
	out := make([]string, tail)
	copy(out, t.recent[len(t.recent)-tail:])
	return out
}

func (t *logTail) markDockerSnapshotPrimed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dockerSnapshotPrimed {
		return false
	}
	t.dockerSnapshotPrimed = true
	return true
}

func splitDockerLogTimestamp(raw string) (string, int64) {
	line := strings.TrimRight(raw, "\r")
	token, rest, ok := strings.Cut(line, " ")
	if !ok || token == "" || rest == "" {
		return line, 0
	}
	if t, err := time.Parse(time.RFC3339Nano, token); err == nil {
		return strings.TrimLeft(rest, " \t"), t.UnixMilli()
	}
	return line, 0
}

func parseDockerLogSnapshotEntries(raw string, tail int) []logSnapshotEntry {
	if raw == "" {
		return nil
	}
	raw = stripDockerStreamFraming(raw)
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	parts := strings.Split(raw, "\n")
	entries := make([]logSnapshotEntry, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimRight(part, "\r")
		if strings.TrimSpace(part) == "" {
			continue
		}
		linePart, ts := splitDockerLogTimestamp(part)
		cleaned := cleanLogLine(linePart)
		if cleaned == "" {
			continue
		}
		for _, line := range strings.Split(cleaned, "\n") {
			line = strings.TrimRight(line, "\r")
			if line == "" {
				continue
			}
			entries = append(entries, logSnapshotEntry{line: line, ts: ts})
		}
	}
	if tail > 0 && len(entries) > tail {
		entries = entries[len(entries)-tail:]
	}
	return entries
}

func parseDockerLogSnapshot(raw string, tail int) []string {
	entries := parseDockerLogSnapshotEntries(raw, tail)
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		lines = append(lines, entry.line)
	}
	return lines
}

func (lm *logManager) fetchTailSnapshotSingleflight(name string, tail int, fetch func() []logSnapshotEntry) []logSnapshotEntry {
	if tail <= 0 || fetch == nil {
		return nil
	}
	key := fmt.Sprintf("%s:%d", name, tail)

	lm.snapshotMu.Lock()
	if call, ok := lm.snapshotInFlight[key]; ok {
		lm.snapshotMu.Unlock()
		<-call.done
		return append([]logSnapshotEntry(nil), call.entries...)
	}
	call := &logSnapshotCall{done: make(chan struct{})}
	lm.snapshotInFlight[key] = call
	lm.snapshotMu.Unlock()

	entries := fetch()

	lm.snapshotMu.Lock()
	call.entries = append([]logSnapshotEntry(nil), entries...)
	close(call.done)
	delete(lm.snapshotInFlight, key)
	lm.snapshotMu.Unlock()

	return append([]logSnapshotEntry(nil), entries...)
}

func (lm *logManager) getOrCreate(name string) *logTail {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	if t, ok := lm.tails[name]; ok {
		return t
	}
	t := &logTail{
		name:    name,
		clients: map[chan string]struct{}{},
		recentN: logTailRecentMaxLines,
		stopCh:  make(chan struct{}),
	}
	lm.tails[name] = t
	go lm.followLoop(t)
	return t
}

func (lm *logManager) removeIfEmpty(t *logTail) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	t.mu.Lock()
	clientCount := len(t.clients)
	alreadyClosed := t.closed
	t.mu.Unlock()

	if clientCount == 0 && !alreadyClosed {
		t.mu.Lock()
		if t.closed {
			t.mu.Unlock()
			return
		}
		t.closed = true
		cmd := t.cmd
		conn := t.conn
		t.mu.Unlock()

		close(t.stopCh)
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		if conn != nil {
			_ = conn.Close()
		}
		delete(lm.tails, t.name)
	}
}

func (lm *logManager) primeDockerLogSnapshot(t *logTail, tail int) {
	if t == nil || tail <= 0 || !t.markDockerSnapshotPrimed() {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	stdout, stderr, _, _ := lm.d.runCollect(ctx, "logs", "--timestamps", "--tail", fmt.Sprint(tail), t.name)
	cancel()

	entries := parseDockerLogSnapshotEntries(stdout, tail)
	for _, entry := range parseDockerLogSnapshotEntries(stderr, tail) {
		lower := strings.ToLower(entry.line)
		if strings.Contains(lower, "no such container") || strings.Contains(lower, "can not get logs from container which is dead") {
			continue
		}
		entries = append(entries, entry)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		left := entries[i].ts
		right := entries[j].ts
		if left == 0 || right == 0 {
			return false
		}
		return left < right
	})
	if len(entries) > tail {
		entries = entries[len(entries)-tail:]
	}
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		lines = append(lines, entry.line)
	}
	t.appendRecentBatch(lines)
}

func (lm *logManager) broadcastLiveLogLine(t *logTail, prefix, raw string) {
	line := cleanLogLine(raw)
	if line == "" {
		return
	}
	lower := strings.ToLower(line)
	if strings.Contains(lower, "no such container") {
		return
	}
	if strings.Contains(lower, "can not get logs from container which is dead") {
		return
	}
	lm.sendToClients(t, prefix+line)
}

func (lm *logManager) followDockerLogsCLI(t *logTail) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "logs", "-f", "--tail", "0", t.name)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	t.mu.Lock()
	t.cmd = cmd
	t.conn = nil
	t.mu.Unlock()

	done := make(chan struct{})
	go func() {
		select {
		case <-t.stopCh:
			cancel()
		case <-done:
		}
	}()

	var wg sync.WaitGroup
	scan := func(r io.Reader, prefix string) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		buf := make([]byte, 0, 64*1024)
		sc.Buffer(buf, 2*1024*1024)
		for sc.Scan() {
			lm.broadcastLiveLogLine(t, prefix, sc.Text())
		}
	}
	wg.Add(2)
	go scan(stdout, "stdout:")
	go scan(stderr, "stderr:")

	err = cmd.Wait()
	close(done)
	wg.Wait()

	t.mu.Lock()
	if t.cmd == cmd {
		t.cmd = nil
	}
	t.mu.Unlock()

	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (lm *logManager) followLoop(t *logTail) {
	backoff := 1 * time.Second
	lastState := ""
	fmt.Printf("[logs] starting follow loop for container: %s\n", t.name)
	for {
		select {
		case <-t.stopCh:
			fmt.Printf("[logs] stop signal received for: %s\n", t.name)
			return
		default:
		}

		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		state := lm.d.containerState(ctx, t.name)
		cancel()
		if state == "" {
			if lastState != "" && lastState != "missing" {
				fmt.Printf("[logs] container %s no longer exists, waiting...\n", t.name)
			}
			lastState = "missing"
			time.Sleep(backoff)
			if backoff < 5*time.Second {
				backoff += 500 * time.Millisecond
			}
			continue
		}

		if state != "running" {
			if state != lastState {
				fmt.Printf("[logs] container %s is %s, waiting for running state before attaching logs\n", t.name, state)
			}
			lastState = state
			time.Sleep(backoff)
			if backoff < 5*time.Second {
				backoff += 500 * time.Millisecond
			}
			continue
		}

		if lastState != "running" {
			fmt.Printf("[logs] container %s running, attaching to live logs\n", t.name)
			lm.primeDockerLogSnapshot(t, 200)
		}
		lastState = "running"
		backoff = 1 * time.Second

		if preferDockerCLIConsoleTransport() {
			if err := lm.followDockerLogsCLI(t); err != nil {
				if lm.debug {
					fmt.Printf("[logs] Docker CLI log stream ended for %s: %v\n", t.name, err)
				}
			}
			select {
			case <-t.stopCh:
				return
			default:
				time.Sleep(500 * time.Millisecond)
				continue
			}
		}

		openStdin := false
		if !preferDockerCLIPTYStdinTransport() {
			openStdinCtx, openStdinCancel := context.WithTimeout(context.Background(), 3*time.Second)
			openStdin = lm.d.containerOpenStdin(openStdinCtx, t.name)
			openStdinCancel()
		}

		attachCtx, attachCancel := context.WithTimeout(context.Background(), 10*time.Second)
		stream, err := lm.d.attachContainerAPI(attachCtx, t.name, dockerAttachOptions{Stdin: openStdin, Stdout: true, Stderr: true})
		attachCancel()
		if err != nil {
			fmt.Printf("[logs] failed to attach live stream for %s: %v\n", t.name, err)
			if cliErr := lm.followDockerLogsCLI(t); cliErr != nil && lm.debug {
				fmt.Printf("[logs] Docker CLI log fallback ended for %s: %v\n", t.name, cliErr)
			}
			select {
			case <-t.stopCh:
				return
			default:
				time.Sleep(1 * time.Second)
				continue
			}
		}

		select {
		case <-t.stopCh:
			_ = stream.Close()
			return
		default:
		}

		t.mu.Lock()
		t.conn = stream
		t.mu.Unlock()

		broadcast := func(r io.Reader) {
			sc := bufio.NewScanner(r)
			buf := make([]byte, 0, 64*1024)
			sc.Buffer(buf, 2*1024*1024)
			for sc.Scan() {
				lm.broadcastLiveLogLine(t, "stdout:", sc.Text())
			}
		}

		broadcast(stream)
		_ = stream.Close()

		t.mu.Lock()
		t.conn = nil
		t.mu.Unlock()

		select {
		case <-t.stopCh:
			return
		default:
			time.Sleep(500 * time.Millisecond)
		}
	}
}

func (lm *logManager) sendToClients(t *logTail, s string) {
	t.mu.Lock()
	t.appendRecentLocked(normalizeStreamedLogLine(s))
	for ch := range t.clients {
		select {
		case ch <- s:
		default:
		}
	}
	t.mu.Unlock()
}

func (lm *logManager) writeCommand(name, command string) error {
	if lm == nil {
		return fmt.Errorf("log manager unavailable")
	}
	dockerName := dockerContainerName(name)
	cmd := strings.TrimSpace(command)
	if dockerName == "" || cmd == "" {
		return fmt.Errorf("missing-params")
	}

	lm.mu.Lock()
	t := lm.tails[dockerName]
	if t == nil && dockerName != name {
		t = lm.tails[name]
	}
	lm.mu.Unlock()
	if t == nil {
		return fmt.Errorf("live console stream unavailable")
	}

	t.mu.Lock()
	conn := t.conn
	closed := t.closed
	t.mu.Unlock()
	if closed || conn == nil {
		return fmt.Errorf("live console stream unavailable")
	}

	line := strings.TrimRight(cmd, "\r\n") + "\n"
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if deadlineWriter, ok := conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
		_ = deadlineWriter.SetWriteDeadline(time.Now().Add(stdinWriteTimeout))
		defer deadlineWriter.SetWriteDeadline(time.Time{})
	}
	if _, err := io.WriteString(conn, line); err != nil {
		_ = conn.Close()
		t.mu.Lock()
		if t.conn == conn {
			t.conn = nil
		}
		t.mu.Unlock()
		return fmt.Errorf("live console stdin write failed: %w", err)
	}
	return nil
}

func (lm *logManager) resetRecent(name string) {
	lm.mu.Lock()
	t, ok := lm.tails[name]
	lm.mu.Unlock()
	if !ok || t == nil {
		return
	}

	t.mu.Lock()
	t.recent = nil
	t.dockerSnapshotPrimed = false
	t.mu.Unlock()
}

func (lm *logManager) killTail(name string) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	t, ok := lm.tails[name]
	if !ok || t == nil {
		return
	}

	t.mu.Lock()
	alreadyClosed := t.closed
	if !alreadyClosed {
		t.closed = true
	}
	cmd := t.cmd
	conn := t.conn
	t.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	if conn != nil {
		_ = conn.Close()
	}

	if !alreadyClosed {
		close(t.stopCh)
	}
	delete(lm.tails, name)
}

func (a *Agent) ensureLiveLogs(name string) {
	if a == nil || a.logs == nil || strings.TrimSpace(name) == "" {
		return
	}
	a.logs.getOrCreate(name)
}

func getDirSizeBytes(dir string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "du", "-sb", dir)
	out, err := cmd.Output()
	if err != nil {
		return getDirSizeBytesGo(dir)
	}

	parts := strings.Fields(string(out))
	if len(parts) < 1 {
		return getDirSizeBytesGo(dir)
	}

	size, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return getDirSizeBytesGo(dir)
	}
	return size, nil
}

func getDirSizeBytesGo(dir string) (int64, error) {
	var size int64
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return size, err
}

type diskMonitor struct {
	agent    *Agent
	interval time.Duration
	stopCh   chan struct{}
	debug    bool
}

func newDiskMonitor(agent *Agent, interval time.Duration, debug bool) *diskMonitor {
	return &diskMonitor{
		agent:    agent,
		interval: interval,
		stopCh:   make(chan struct{}),
		debug:    debug,
	}
}

func (dm *diskMonitor) start() {
	go dm.monitorLoop()
}

func (dm *diskMonitor) stop() {
	close(dm.stopCh)
}

func (dm *diskMonitor) monitorLoop() {
	time.Sleep(10 * time.Second)

	ticker := time.NewTicker(dm.interval)
	defer ticker.Stop()

	for {
		select {
		case <-dm.stopCh:
			return
		case <-ticker.C:
			dm.checkAllServers()
		}
	}
}

func (dm *diskMonitor) checkAllServers() {
	entries, err := os.ReadDir(dm.agent.volumesDir)
	if err != nil {
		if dm.debug {
			fmt.Fprintf(os.Stderr, "[disk-monitor] error reading volumes dir: %v\n", err)
		}
		return
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}

		serverName := e.Name()
		serverDir := dm.agent.serverDir(serverName)

		meta := dm.agent.loadMeta(serverDir)
		resources, ok := meta["resources"].(map[string]any)
		if !ok {
			continue
		}

		var limitBytes int64

		if val, ok := resources["storageMb"]; ok {
			var limitMb int64
			switch v := val.(type) {
			case float64:
				limitMb = int64(v)
			case int:
				limitMb = int64(v)
			case int64:
				limitMb = v
			}
			if limitMb > 0 {
				limitBytes = limitMb * 1024 * 1024
			}
		}

		if limitBytes == 0 {
			if val, ok := resources["storageGb"]; ok {
				var limitGb int64
				switch v := val.(type) {
				case float64:
					limitGb = int64(v)
				case int:
					limitGb = int64(v)
				case int64:
					limitGb = v
				}
				if limitGb > 0 {
					limitBytes = limitGb * 1024 * 1024 * 1024
				}
			}
		}

		if limitBytes <= 0 {
			continue
		}

		usedBytes, err := getDirSizeBytes(serverDir)
		if err != nil {
			if dm.debug {
				fmt.Fprintf(os.Stderr, "[disk-monitor] error getting size for %s: %v\n", serverName, err)
			}
			continue
		}

		if dm.debug {
			usedGb := float64(usedBytes) / (1024 * 1024 * 1024)
			limitGb := float64(limitBytes) / (1024 * 1024 * 1024)
			fmt.Printf("[disk-monitor] %s: %.2f GB / %.2f GB\n", serverName, usedGb, limitGb)
		}

		if usedBytes > limitBytes {
			dm.handleOverLimit(serverName, usedBytes, limitBytes)
		}
	}
}

func (dm *diskMonitor) handleOverLimit(serverName string, usedBytes, limitBytes int64) {
	usedGb := float64(usedBytes) / (1024 * 1024 * 1024)
	limitGb := float64(limitBytes) / (1024 * 1024 * 1024)

	fmt.Printf("[disk-monitor] Server %s exceeded disk limit: %.2f GB / %.2f GB - stopping container\n",
		serverName, usedGb, limitGb)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if !dm.agent.docker.containerExists(ctx, serverName) {
		return
	}

	out, _, _, err := dm.agent.docker.runCollect(ctx, "inspect", "-f", "{{.State.Running}}", serverName)
	if err != nil || strings.TrimSpace(out) != "true" {
		return
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCancel()

	_, _, _, err = dm.agent.docker.runCollect(stopCtx, "stop", "-t", "30", serverName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[disk-monitor] error stopping %s: %v\n", serverName, err)
	} else {
		fmt.Printf("[disk-monitor] Container %s stopped due to disk limit exceeded\n", serverName)
	}
}

type networkAbuseMonitor struct {
	agent    *Agent
	interval time.Duration
	workers  int
	stopCh   chan struct{}
	debug    bool
}

func newNetworkAbuseMonitor(agent *Agent, interval time.Duration, debug bool) *networkAbuseMonitor {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if v := os.Getenv("NETWORK_ABUSE_MONITOR_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1000 && n <= 60000 {
			interval = time.Duration(n) * time.Millisecond
		}
	}

	workers := runtime.NumCPU() * 2
	if workers < 4 {
		workers = 4
	}
	if workers > 16 {
		workers = 16
	}
	if v := os.Getenv("NETWORK_ABUSE_MONITOR_WORKERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 64 {
			workers = n
		}
	}

	return &networkAbuseMonitor{
		agent:    agent,
		interval: interval,
		workers:  workers,
		stopCh:   make(chan struct{}),
		debug:    debug,
	}
}

func (nm *networkAbuseMonitor) start() {
	go nm.monitorLoop()
}

func (nm *networkAbuseMonitor) stop() {
	close(nm.stopCh)
}

func (nm *networkAbuseMonitor) monitorLoop() {
	time.Sleep(12 * time.Second)

	ticker := time.NewTicker(nm.interval)
	defer ticker.Stop()

	for {
		select {
		case <-nm.stopCh:
			return
		default:
			nm.checkAllServers()
		}

		select {
		case <-nm.stopCh:
			return
		case <-ticker.C:
		}
	}
}

func (nm *networkAbuseMonitor) checkAllServers() {
	entries, err := os.ReadDir(nm.agent.volumesDir)
	if err != nil {
		if nm.debug {
			fmt.Fprintf(os.Stderr, "[network-abuse] error reading volumes dir: %v\n", err)
		}
		return
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		return
	}

	workers := nm.workers
	if workers > len(names) {
		workers = len(names)
	}
	jobs := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for name := range jobs {
				nm.checkServer(name)
			}
		}()
	}

	for _, name := range names {
		select {
		case <-nm.stopCh:
			close(jobs)
			wg.Wait()
			return
		case jobs <- name:
		}
	}
	close(jobs)
	wg.Wait()
}

func (nm *networkAbuseMonitor) checkServer(serverName string) {
	if !isValidContainerName(serverName) || nm.agent == nil {
		return
	}

	serverDir := nm.agent.serverDir(serverName)
	meta := nm.agent.loadMeta(serverDir)
	rule := networkAntiAbuseRuleFromMeta(meta)
	if !rule.enabled {
		return
	}

	inspectCtx, inspectCancel := context.WithTimeout(context.Background(), 3500*time.Millisecond)
	inspected, exists, err := nm.agent.inspectContainerForStats(inspectCtx, serverName)
	inspectCancel()
	if err != nil || !exists || !inspected.State.Running {
		if nm.debug && err != nil {
			fmt.Fprintf(os.Stderr, "[network-abuse] inspect %s failed: %v\n", serverName, err)
		}
		return
	}

	statsCtx, statsCancel := context.WithTimeout(context.Background(), 3500*time.Millisecond)
	stats, err := nm.agent.readContainerStatsForServer(statsCtx, serverName)
	statsCancel()
	if err != nil {
		if nm.debug {
			fmt.Fprintf(os.Stderr, "[network-abuse] stats %s failed: %v\n", serverName, err)
		}
		return
	}
	if len(stats.Networks) == 0 {
		return
	}

	rawRx, rawTx := sumDockerNetworkBytes(stats.Networks)
	if nm.agent.resourceStats == nil {
		return
	}
	entry := nm.agent.resourceStats.get(serverName)
	now := time.Now()
	entry.mu.Lock()
	entry.touchedAt = now
	_, _, windowRx, windowTx, _ := entry.updateNetworkCounters(inspected.ID, rawRx, rawTx, now, rule.resetEvery)
	entry.mu.Unlock()

	inboundExceeded := rule.inboundLimitBytes > 0 && windowRx >= rule.inboundLimitBytes
	outboundExceeded := rule.outboundLimitBytes > 0 && windowTx >= rule.outboundLimitBytes
	if !inboundExceeded && !outboundExceeded {
		return
	}

	reason := "network limit reached"
	if inboundExceeded && outboundExceeded {
		reason = "inbound and outbound network limits reached"
	} else if inboundExceeded {
		reason = "inbound network limit reached"
	} else if outboundExceeded {
		reason = "outbound network limit reached"
	}
	if nm.debug {
		fmt.Printf("[network-abuse] %s exceeded limit: inbound=%d/%d outbound=%d/%d\n",
			serverName, windowRx, rule.inboundLimitBytes, windowTx, rule.outboundLimitBytes)
	}
	if err := nm.agent.stopServerForPolicy(serverName, reason); err != nil && nm.debug {
		fmt.Fprintf(os.Stderr, "[network-abuse] stop %s failed: %v\n", serverName, err)
	}
}

func (a *Agent) stopServerForPolicy(name, reason string) error {
	if !isValidContainerName(name) {
		return fmt.Errorf("invalid container name")
	}

	opMu := a.getServerOpLock(name)
	opMu.Lock()
	defer opMu.Unlock()

	a.stopMu.Lock()
	if lastStop, ok := a.stoppingNow[name]; ok && time.Since(lastStop) < 5*time.Second {
		a.stopMu.Unlock()
		return nil
	}
	a.stoppingNow[name] = time.Now()
	a.stopMu.Unlock()
	defer func() {
		a.stopMu.Lock()
		delete(a.stoppingNow, name)
		a.stopMu.Unlock()
	}()

	inspectCtx, inspectCancel := context.WithTimeout(context.Background(), 5*time.Second)
	inspected, exists, err := a.inspectContainerForStats(inspectCtx, name)
	inspectCancel()
	if err != nil {
		return err
	}
	if !exists || !inspected.State.Running {
		return nil
	}

	updateCtx, updateCancel := context.WithTimeout(context.Background(), 5*time.Second)
	a.docker.updateRestartNo(updateCtx, name)
	updateCancel()

	if a.stdinMgr != nil {
		a.stdinMgr.remove(name)
	}

	serverDir := a.serverDir(name)
	meta := a.loadMeta(serverDir)
	serverType := strings.ToLower(strings.TrimSpace(fmt.Sprint(meta["type"])))
	templateType := strings.ToLower(strings.TrimSpace(fmt.Sprint(meta["template"])))
	isMinecraft := serverType == "" || serverType == "minecraft" || templateType == "minecraft"

	if isMinecraft {
		commandCtx, commandCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = a.docker.sendCommand(commandCtx, a.stdinMgr, name, "stop", nil)
		commandCancel()
	}

	a.broadcastContainerEvent(name, "stopping")
	time.Sleep(a.stopGrace)

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	a.docker.stop(stopCtx, name)
	stopCancel()
	if a.stdinMgr != nil {
		a.stdinMgr.remove(name)
	}

	a.broadcastContainerEvent(name, "stopped")
	fmt.Printf("[network-abuse] Container %s stopped safely: %s\n", name, reason)
	auditLog.Log("server_stop", "node-agent", "", name, "network-abuse-stop", "success", map[string]any{"reason": reason})
	return nil
}

func (a *Agent) getServerDiskUsage(serverDir string) int64 {
	size, err := getDirSizeBytes(serverDir)
	if err != nil {
		return 0
	}
	return size
}

func (a *Agent) getServerDiskLimitBytes(serverDir string) int64 {
	meta := a.loadMeta(serverDir)
	resources, ok := meta["resources"].(map[string]any)
	if !ok {
		return 0
	}
	if val, ok := resources["storageMb"]; ok {
		var limitMb int64
		switch v := val.(type) {
		case float64:
			limitMb = int64(v)
		case int:
			limitMb = int64(v)
		case int64:
			limitMb = v
		}
		if limitMb > 0 {
			return limitMb * 1024 * 1024
		}
	}
	if val, ok := resources["storageGb"]; ok {
		var limitGb int64
		switch v := val.(type) {
		case float64:
			limitGb = int64(v)
		case int:
			limitGb = int64(v)
		case int64:
			limitGb = v
		}
		if limitGb > 0 {
			return limitGb * 1024 * 1024 * 1024
		}
	}
	return 0
}

func (a *Agent) checkDiskLimit(serverDir string, extraBytes int64) map[string]any {
	limitBytes := a.getServerDiskLimitBytes(serverDir)
	if limitBytes <= 0 {
		return nil
	}
	usedBytes := a.getServerDiskUsage(serverDir)
	if usedBytes+extraBytes > limitBytes {
		usedMb := float64(usedBytes) / (1024 * 1024)
		limitMb := float64(limitBytes) / (1024 * 1024)
		return map[string]any{
			"error":      "disk_limit_exceeded",
			"message":    fmt.Sprintf("This server has exceeded its disk space limit. Used: %.1f MB / %.1f MB", usedMb, limitMb),
			"usedBytes":  usedBytes,
			"limitBytes": limitBytes,
			"usedMb":     usedMb,
			"limitMb":    limitMb,
		}
	}
	return nil
}

func (a *Agent) serverNameFromPath(absPath string) string {
	rel, err := filepath.Rel(a.volumesDir, absPath)
	if err != nil {
		return ""
	}
	parts := strings.SplitN(filepath.ToSlash(rel), "/", 2)
	if len(parts) == 0 || parts[0] == "" || parts[0] == "." || parts[0] == ".." {
		return ""
	}
	return parts[0]
}

func (a *Agent) checkDiskLimitForPath(absPath string, extraBytes int64) map[string]any {
	serverName := a.serverNameFromPath(absPath)
	if serverName == "" || serverName == ".adpanel_meta" {
		return nil
	}
	serverDir := a.serverDir(serverName)
	return a.checkDiskLimit(serverDir, extraBytes)
}

func (a *Agent) checkDiskLimitForPathAfterReplacing(absPath string, replacedBytes int64) map[string]any {
	serverName := a.serverNameFromPath(absPath)
	if serverName == "" || serverName == ".adpanel_meta" {
		return nil
	}
	serverDir := a.serverDir(serverName)
	limitBytes := a.getServerDiskLimitBytes(serverDir)
	if limitBytes <= 0 {
		return nil
	}
	usedBytes := a.getServerDiskUsage(serverDir)
	finalUsedBytes := usedBytes - replacedBytes
	if finalUsedBytes < 0 {
		finalUsedBytes = 0
	}
	if finalUsedBytes > limitBytes {
		usedMb := float64(finalUsedBytes) / (1024 * 1024)
		limitMb := float64(limitBytes) / (1024 * 1024)
		return map[string]any{
			"error":      "disk_limit_exceeded",
			"message":    fmt.Sprintf("This server has exceeded its disk space limit. Used: %.1f MB / %.1f MB", usedMb, limitMb),
			"usedBytes":  finalUsedBytes,
			"limitBytes": limitBytes,
			"usedMb":     usedMb,
			"limitMb":    limitMb,
		}
	}
	return nil
}

func diskLimitUploadPayload(usedBytes, limitBytes, incomingBytes int64) map[string]any {
	if usedBytes < 0 {
		usedBytes = 0
	}
	if incomingBytes < 0 {
		incomingBytes = 0
	}
	remainingBytes := limitBytes - usedBytes
	if remainingBytes < 0 {
		remainingBytes = 0
	}
	usedMb := float64(usedBytes) / (1024 * 1024)
	limitMb := float64(limitBytes) / (1024 * 1024)
	return map[string]any{
		"error":          "disk_limit_exceeded",
		"message":        fmt.Sprintf("Upload would exceed this server's disk space limit. Used: %.1f MB / %.1f MB", usedMb, limitMb),
		"usedBytes":      usedBytes,
		"limitBytes":     limitBytes,
		"remainingBytes": remainingBytes,
		"incomingBytes":  incomingBytes,
		"usedMb":         usedMb,
		"limitMb":        limitMb,
	}
}

func (a *Agent) uploadDiskAllowanceForPath(absPath string, replacedBytes int64) (allowedBytes, baseUsedBytes, limitBytes int64, limited bool, diskErr map[string]any) {
	serverName := a.serverNameFromPath(absPath)
	if serverName == "" || serverName == ".adpanel_meta" {
		return 0, 0, 0, false, nil
	}
	serverDir := a.serverDir(serverName)
	limitBytes = a.getServerDiskLimitBytes(serverDir)
	if limitBytes <= 0 {
		return 0, 0, 0, false, nil
	}
	usedBytes := a.getServerDiskUsage(serverDir)
	baseUsedBytes = usedBytes - replacedBytes
	if baseUsedBytes < 0 {
		baseUsedBytes = 0
	}
	if baseUsedBytes > limitBytes {
		return 0, baseUsedBytes, limitBytes, true, diskLimitUploadPayload(baseUsedBytes, limitBytes, 0)
	}
	return limitBytes - baseUsedBytes, baseUsedBytes, limitBytes, true, nil
}

func copyUploadWithLimits(dst io.Writer, src io.Reader, maxBytes int64, diskAllowedBytes int64) (int64, string, error) {
	const maxInt64 = int64(^uint64(0) >> 1)
	if maxBytes <= 0 {
		maxBytes = maxInt64
	}
	buf := make([]byte, 64*1024)
	var written int64
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			next := written + int64(n)
			if next > maxBytes {
				return written, "upload", nil
			}
			if diskAllowedBytes >= 0 && next > diskAllowedBytes {
				return written, "disk", nil
			}
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return written, "", writeErr
			}
			written = next
		}
		if readErr == io.EOF {
			return written, "", nil
		}
		if readErr != nil {
			return written, "", readErr
		}
	}
}

func protectNodeProcess() {
	pid := os.Getpid()
	path := fmt.Sprintf("/proc/%d/oom_score_adj", pid)
	err := os.WriteFile(path, []byte("-1000"), 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[oom-protect] WARNING: could not set oom_score_adj to -1000: %v\n", err)
		fmt.Fprintln(os.Stderr, "[oom-protect] The node process may be killed by Linux OOM killer under memory pressure.")
		fmt.Fprintln(os.Stderr, "[oom-protect] Ensure this process runs as root or has CAP_SYS_RESOURCE.")
	} else {
		fmt.Printf("[oom-protect] Node process (PID %d) protected with oom_score_adj=-1000\n", pid)
	}
}

type containerMemState struct {
	state   string
	since   time.Time
	lastPct float64
}

type memoryMonitor struct {
	agent    *Agent
	interval time.Duration
	stopCh   chan struct{}
	debug    bool

	recentKills   map[string]time.Time
	recentKillsMu sync.Mutex

	containerStates map[string]*containerMemState
	statesMu        sync.Mutex
}

func newMemoryMonitor(agent *Agent, interval time.Duration, debug bool) *memoryMonitor {
	return &memoryMonitor{
		agent:           agent,
		interval:        interval,
		stopCh:          make(chan struct{}),
		debug:           debug,
		recentKills:     make(map[string]time.Time),
		containerStates: make(map[string]*containerMemState),
	}
}

func (mm *memoryMonitor) start() {
	go mm.monitorLoop()
}

func (mm *memoryMonitor) stop() {
	close(mm.stopCh)
}

func (mm *memoryMonitor) monitorLoop() {
	time.Sleep(15 * time.Second)

	ticker := time.NewTicker(mm.interval)
	defer ticker.Stop()

	for {
		select {
		case <-mm.stopCh:
			return
		case <-ticker.C:
			mm.tick()
		}
	}
}

func (mm *memoryMonitor) tick() {
	totalKB, availKB, err := readSystemMemory()
	if err != nil {
		if mm.debug {
			fmt.Fprintf(os.Stderr, "[mem-monitor] error reading /proc/meminfo: %v\n", err)
		}
		return
	}

	if totalKB == 0 {
		return
	}

	usedPercent := float64(totalKB-availKB) / float64(totalKB) * 100.0

	if mm.debug {
		fmt.Printf("[mem-monitor] System memory: %.1f%% used (%d MB / %d MB)\n",
			usedPercent,
			(totalKB-availKB)/1024,
			totalKB/1024)
	}

	hostTotalBytes := int64(totalKB) * 1024

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, _, _, err := mm.agent.docker.runCollect(ctx, "ps", "--no-trunc", "--format", "{{.ID}}|{{.Names}}")
	if err != nil {
		if mm.debug {
			fmt.Fprintf(os.Stderr, "[mem-monitor] docker ps error: %v\n", err)
		}
		return
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		return
	}

	mm.pruneRecentKills()

	activeIDs := make(map[string]struct{}, len(lines))

	for _, line := range lines {
		parts := strings.SplitN(strings.TrimSpace(line), "|", 2)
		if len(parts) != 2 || parts[0] == "" {
			continue
		}
		containerID := parts[0]
		containerName := parts[1]
		activeIDs[containerID] = struct{}{}

		mm.recentKillsMu.Lock()
		if _, killed := mm.recentKills[containerID]; killed {
			mm.recentKillsMu.Unlock()
			continue
		}
		mm.recentKillsMu.Unlock()

		usageBytes, limitBytes := mm.readContainerCgroupMemory(containerID)
		if usageBytes < 0 {
			continue
		}

		effectiveLimit := limitBytes
		isUnlimited := false
		if limitBytes <= 0 || limitBytes >= hostTotalBytes {
			effectiveLimit = hostTotalBytes
			isUnlimited = true
		}

		containerUsedPct := float64(usageBytes) / float64(effectiveLimit) * 100.0

		if mm.debug && containerUsedPct > 75.0 {
			limitLabel := fmt.Sprintf("%d MB", effectiveLimit/(1024*1024))
			if isUnlimited {
				limitLabel = fmt.Sprintf("%d MB (host RAM)", effectiveLimit/(1024*1024))
			}
			fmt.Printf("[mem-monitor] Container %s: %.1f%% memory (%d MB / %s)\n",
				containerName, containerUsedPct,
				usageBytes/(1024*1024), limitLabel)
		}

		if containerUsedPct >= 99.0 {
			idShort := containerID
			if len(idShort) > 12 {
				idShort = idShort[:12]
			}
			limitDesc := fmt.Sprintf("%d MB", effectiveLimit/(1024*1024))
			if isUnlimited {
				limitDesc = fmt.Sprintf("%d MB (host RAM, unlimited server)", effectiveLimit/(1024*1024))
			}
			fmt.Printf("[mem-monitor] Container %s (%s) at %.1f%% of memory limit (%d MB / %s) — FORCE KILLING\n",
				containerName, idShort, containerUsedPct,
				usageBytes/(1024*1024), limitDesc)

			mm.killContainer(containerID, containerName)
			mm.setContainerState(containerID, "normal")
			continue
		}

		if containerUsedPct >= 96.0 {
			state := mm.getContainerState(containerID)

			if state == nil || state.state != "stopping" {
				idShort := containerID
				if len(idShort) > 12 {
					idShort = idShort[:12]
				}
				limitDesc := fmt.Sprintf("%d MB", effectiveLimit/(1024*1024))
				if isUnlimited {
					limitDesc = fmt.Sprintf("%d MB (host RAM, unlimited server)", effectiveLimit/(1024*1024))
				}
				fmt.Printf("[mem-monitor] Container %s (%s) at %.1f%% of memory limit (%d MB / %s) — graceful stop\n",
					containerName, idShort, containerUsedPct,
					usageBytes/(1024*1024), limitDesc)

				mm.gracefulStopContainer(containerID, containerName)
				mm.setContainerState(containerID, "stopping")
			} else if mm.debug {
				fmt.Printf("[mem-monitor] Container %s still at %.1f%% — graceful stop already issued, waiting for 99%% to force kill\n",
					containerName, containerUsedPct)
			}

			mm.updateContainerPct(containerID, containerUsedPct)
			continue
		}

		if containerUsedPct < 90.0 {
			state := mm.getContainerState(containerID)
			if state != nil && state.state != "normal" {
				if mm.debug {
					fmt.Printf("[mem-monitor] Container %s recovered to %.1f%% — clearing warning state\n",
						containerName, containerUsedPct)
				}
				mm.setContainerState(containerID, "normal")
			}
		}
	}

	mm.pruneContainerStates(activeIDs)
}

func readSystemMemory() (totalKB, availKB int64, err error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	var memTotal, memAvailable, memFree, buffers, cached int64
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		val, _ := strconv.ParseInt(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			memTotal = val
		case "MemAvailable:":
			memAvailable = val
		case "MemFree:":
			memFree = val
		case "Buffers:":
			buffers = val
		case "Cached:":
			cached = val
		}
	}
	if memAvailable > 0 {
		return memTotal, memAvailable, nil
	}
	return memTotal, memFree + buffers + cached, nil
}

func (mm *memoryMonitor) readContainerCgroupMemory(containerID string) (usageBytes, limitBytes int64) {
	cgroupV2Base := fmt.Sprintf("/sys/fs/cgroup/system.slice/docker-%s.scope", containerID)

	usage := readInt64File(cgroupV2Base + "/memory.current")
	limit := readInt64File(cgroupV2Base + "/memory.max")
	if usage >= 0 && limit > 0 {
		return usage, limit
	}

	cgroupV2Alt := fmt.Sprintf("/sys/fs/cgroup/docker/%s", containerID)
	usage = readInt64File(cgroupV2Alt + "/memory.current")
	limit = readInt64File(cgroupV2Alt + "/memory.max")
	if usage >= 0 && limit > 0 {
		return usage, limit
	}

	cgroupV1Base := fmt.Sprintf("/sys/fs/cgroup/memory/docker/%s", containerID)
	usage = readInt64File(cgroupV1Base + "/memory.usage_in_bytes")
	limit = readInt64File(cgroupV1Base + "/memory.limit_in_bytes")
	if usage >= 0 && limit > 0 {
		return usage, limit
	}

	cgroupV1Sys := fmt.Sprintf("/sys/fs/cgroup/memory/system.slice/docker-%s.scope", containerID)
	usage = readInt64File(cgroupV1Sys + "/memory.usage_in_bytes")
	limit = readInt64File(cgroupV1Sys + "/memory.limit_in_bytes")
	if usage >= 0 && limit > 0 {
		return usage, limit
	}

	return -1, -1
}

func readInt64File(path string) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	s := strings.TrimSpace(string(data))
	if s == "max" || s == "" {
		return -1
	}
	val, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return -1
	}
	return val
}

func (mm *memoryMonitor) gracefulStopContainer(containerID, containerName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	idShort := containerID
	if len(idShort) > 12 {
		idShort = idShort[:12]
	}

	_, errStr, _, err := mm.agent.docker.runCollect(ctx, "stop", "-t", "15", containerID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[mem-monitor] failed to gracefully stop container %s (%s): %v — %s\n",
			containerName, idShort, err, strings.TrimSpace(errStr))
		return
	}

	fmt.Printf("[mem-monitor] Gracefully stopped container %s (%s) due to high memory usage\n",
		containerName, idShort)

	mm.recentKillsMu.Lock()
	mm.recentKills[containerID] = time.Now()
	mm.recentKillsMu.Unlock()
}

func (mm *memoryMonitor) killContainer(containerID, containerName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	idShort := containerID
	if len(idShort) > 12 {
		idShort = idShort[:12]
	}

	_, errStr, _, err := mm.agent.docker.runCollect(ctx, "kill", containerID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[mem-monitor] failed to kill container %s (%s): %v — %s\n",
			containerName, idShort, err, strings.TrimSpace(errStr))
		return
	}

	fmt.Printf("[mem-monitor] Force killed container %s (%s) due to critical memory overuse\n",
		containerName, idShort)

	mm.recentKillsMu.Lock()
	mm.recentKills[containerID] = time.Now()
	mm.recentKillsMu.Unlock()
}

func (mm *memoryMonitor) getContainerState(containerID string) *containerMemState {
	mm.statesMu.Lock()
	defer mm.statesMu.Unlock()
	return mm.containerStates[containerID]
}

func (mm *memoryMonitor) setContainerState(containerID, state string) {
	mm.statesMu.Lock()
	defer mm.statesMu.Unlock()
	mm.containerStates[containerID] = &containerMemState{
		state: state,
		since: time.Now(),
	}
}

func (mm *memoryMonitor) updateContainerPct(containerID string, pct float64) {
	mm.statesMu.Lock()
	defer mm.statesMu.Unlock()
	if s, ok := mm.containerStates[containerID]; ok {
		s.lastPct = pct
	}
}

func (mm *memoryMonitor) pruneContainerStates(activeIDs map[string]struct{}) {
	mm.statesMu.Lock()
	defer mm.statesMu.Unlock()
	for id := range mm.containerStates {
		if _, active := activeIDs[id]; !active {
			delete(mm.containerStates, id)
		}
	}
}

func (mm *memoryMonitor) pruneRecentKills() {
	mm.recentKillsMu.Lock()
	defer mm.recentKillsMu.Unlock()

	cutoff := time.Now().Add(-60 * time.Second)
	for id, t := range mm.recentKills {
		if t.Before(cutoff) {
			delete(mm.recentKills, id)
		}
	}
}

type rateLimiter struct {
	mu       sync.Mutex
	attempts map[string]*rateLimitEntry
	limit    int
	window   time.Duration
	maxSize  int
}

type rateLimitEntry struct {
	count   int
	resetAt time.Time
}

var trustProxyHeaders = false

var authRateLimiter = &rateLimiter{
	attempts: make(map[string]*rateLimitEntry),
	limit:    100,
	window:   5 * time.Minute,
	maxSize:  50000,
}

func (rl *rateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()

	if len(rl.attempts) >= rl.maxSize {
		for k, v := range rl.attempts {
			if now.After(v.resetAt) {
				delete(rl.attempts, k)
			}
		}

		if len(rl.attempts) >= rl.maxSize {
			var oldestKey string
			var oldestTime time.Time
			first := true
			for k, v := range rl.attempts {
				if first || v.resetAt.Before(oldestTime) {
					oldestKey = k
					oldestTime = v.resetAt
					first = false
				}
			}
			if oldestKey != "" {
				delete(rl.attempts, oldestKey)
			}
		}
	}

	entry, exists := rl.attempts[ip]

	if !exists || now.After(entry.resetAt) {
		rl.attempts[ip] = &rateLimitEntry{count: 1, resetAt: now.Add(rl.window)}
		return true
	}

	entry.count++
	if entry.count > rl.limit {
		return false
	}
	return true
}

var rateLimitCleanupOnce sync.Once

func startRateLimitCleanup() {
	rateLimitCleanupOnce.Do(func() {
		go func() {
			for {
				time.Sleep(time.Minute)
				authRateLimiter.mu.Lock()
				now := time.Now()
				for ip, entry := range authRateLimiter.attempts {
					if now.After(entry.resetAt) {
						delete(authRateLimiter.attempts, ip)
					}
				}
				authRateLimiter.mu.Unlock()
			}
		}()
	})
}

func getClientIP(r *http.Request) string {
	if trustProxyHeaders {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if len(parts) > 0 {
				return strings.TrimSpace(parts[0])
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (a *Agent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-XSS-Protection", "1; mode=block")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	w.Header().Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	if r.TLS != nil {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	}

	path := r.URL.Path
	method := r.Method

	if method == "HEAD" && path == "/ping" {
		w.WriteHeader(204)
		return
	}
	if method == "GET" && path == "/health" {
		jsonWrite(w, 200, map[string]any{
			"ok":      true,
			"version": NodeVersion,
			"time":    time.Now().UnixMilli(),
		})
		return
	}

	if strings.HasPrefix(path, "/v1/servers/") {
		rest := strings.TrimPrefix(path, "/v1/servers/")
		parts := strings.Split(rest, "/")
		if len(parts) >= 2 {
			name := sanitizeName(parts[0])
			action := parts[1]
			if name == "" {
				jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid name"})
				return
			}
			if method == "POST" && action == "apply-version" {
				a.handleApplyVersion(w, r, name)
				return
			}
		}
	}

	clientIP := getClientIP(r)

	if !a.authOK(r) {
		if !authRateLimiter.allow(clientIP) {
			auditLog.Log("rate_limit", clientIP, "", path, method, "blocked", nil)
			jsonWrite(w, 429, map[string]any{"error": "too many requests"})
			return
		}
		auditLog.Log("auth_failure", clientIP, "", path, method, "denied", nil)
		jsonWrite(w, 401, map[string]any{"error": "unauthorized"})
		return
	}

	switch {
	case method == "GET" && path == "/api/system/stats":
		a.handleSystemStats(w, r)
		return

	case method == "GET" && path == "/v1/info":
		jsonWrite(w, 200, map[string]any{
			"ok": true,
			"node": map[string]any{
				"uuid":       a.uuid,
				"version":    NodeVersion,
				"autoUpdate": a.readNodeInformation().NodeAutoUpdate,
				"dataRoot":   a.dataRoot,
				"volumesDir": a.volumesDir,
			},
		})
		return

	case method == "GET" && path == "/v1/ports/check":
		a.handlePortCheck(w, r)
		return

	case method == "GET" && path == "/v1/update/info":
		a.handleNodeUpdateInfo(w, r)
		return

	case method == "POST" && path == "/v1/update/auto":
		a.handleNodeUpdateAuto(w, r)
		return

	case method == "POST" && path == "/v1/update/install":
		a.handleNodeUpdateInstall(w, r)
		return

	case method == "GET" && path == "/v1/ws":
		a.handleWebSocket(w, r)
		return

	case method == "GET" && path == "/v1/servers":
		a.handleListServers(w, r)
		return

	case method == "POST" && (path == "/v1/servers" || path == "/v1/servers/create"):
		a.handleCreateServer(w, r)
		return

	case method == "POST" && path == "/v1/runtime/startup-command":
		a.handleTemplateStartupCommand(w, r)
		return

	case method == "POST" && path == "/v1/fs/list":
		a.handleFSList(w, r)
		return
	case method == "POST" && path == "/v1/fs/read":
		a.handleFSRead(w, r)
		return
	case method == "POST" && path == "/v1/fs/download":
		a.handleFSDownload(w, r)
		return
	case method == "POST" && path == "/v1/fs/write":
		a.handleFSWrite(w, r)
		return
	case method == "POST" && path == "/v1/fs/delete":
		a.handleFSDelete(w, r)
		return
	case method == "POST" && path == "/v1/fs/delete-batch":
		a.handleFSDeleteBatch(w, r)
		return
	case method == "POST" && path == "/v1/fs/rename":
		a.handleFSRename(w, r)
		return
	case method == "POST" && path == "/v1/fs/extract":
		a.handleFSExtract(w, r)
		return
	case method == "POST" && path == "/v1/fs/uploadRaw":
		a.handleFSUploadRaw(w, r)
		return
	case method == "POST" && path == "/v1/fs/upload":
		a.handleFSUploadStream(w, r)
		return
	case method == "POST" && path == "/v1/fs/uploadUrl":
		a.handleFSUploadURL(w, r)
		return

	case method == "POST" && path == "/v1/server/command":
		a.handleServerCommandBody(w, r)
		return
	}

	if strings.HasPrefix(path, "/v1/servers/") {
		rest := strings.TrimPrefix(path, "/v1/servers/")
		parts := strings.Split(rest, "/")
		if len(parts) >= 2 {
			name := sanitizeName(parts[0])
			action := parts[1]
			if name == "" {
				jsonWrite(w, 400, map[string]any{"error": "invalid name"})
				return
			}
			switch action {
			case "":
			case "runtime":
				if method == "POST" {
					a.handleRuntime(w, r, name)
					return
				}
			case "start":
				if method == "POST" {
					a.handleStart(w, r, name)
					return
				}
			case "stop":
				if method == "POST" {
					a.handleStop(w, r, name)
					return
				}
			case "restart":
				if method == "POST" {
					a.handleRestart(w, r, name)
					return
				}
			case "command":
				if method == "POST" {
					a.handleCommand(w, r, name)
					return
				}
			case "logs":
				if method == "GET" {
					a.handleLogsSSE(w, r, name)
					return
				}
			case "kill":
				if method == "POST" {
					a.handleKill(w, r, name)
					return
				}
			case "delete-status":
				if method == "GET" {
					a.handleDeleteStatus(w, r, name)
					return
				}
			case "files":
				if len(parts) >= 3 {
					sub := parts[2]
					switch sub {
					case "list":
						if method == "GET" {
							a.handleFilesList(w, r, name)
							return
						}
					case "read":
						if method == "GET" {
							a.handleFilesRead(w, r, name)
							return
						}
					case "write":
						if method == "PUT" {
							a.handleFilesWrite(w, r, name)
							return
						}
					case "delete":
						if method == "DELETE" {
							a.handleFilesDelete(w, r, name)
							return
						}
					case "upload":
						if method == "POST" {
							a.handleFilesUpload(w, r, name)
							return
						}
					case "archive":
						if method == "POST" {
							a.handleFilesArchive(w, r, name)
							return
						}
					}
				}
			case "backups":
				if len(parts) == 2 {
					if method == "GET" {
						a.handleBackupsList(w, r, name)
						return
					}
					if method == "POST" {
						a.handleBackupsCreate(w, r, name)
						return
					}
				} else if len(parts) >= 3 {
					backupId := parts[2]
					if len(parts) == 3 {
						if method == "DELETE" {
							a.handleBackupsDelete(w, r, name, backupId)
							return
						}
					} else if len(parts) == 4 && parts[3] == "restore" {
						if method == "POST" {
							a.handleBackupsRestore(w, r, name, backupId)
							return
						}
					}
				}
			case "subdomains":
				if method == "POST" {
					a.handleSubdomains(w, r, name)
					return
				}
			case "resources":
				if method == "GET" {
					a.handleGetResources(w, r, name)
					return
				}
				if method == "PATCH" {
					a.handleUpdateResources(w, r, name)
					return
				}
			case "port-forwards":
				if method == "GET" {
					a.handleGetPortForwards(w, r, name)
					return
				}
				if method == "PATCH" {
					a.handleUpdatePortForwards(w, r, name)
					return
				}
			case "extract-image":
				if method == "POST" {
					a.handleExtractImage(w, r, name)
					return
				}
			case "import-archive":
				if method == "POST" && len(parts) == 2 {
					a.handleImportArchive(w, r, name)
					return
				}
				if method == "GET" && len(parts) == 3 && parts[2] == "progress" {
					a.handleImportArchiveProgress(w, r, name)
					return
				}
			case "export":
				if method == "GET" {
					a.handleExportServer(w, r, name)
					return
				}
			case "import-tar":
				if method == "POST" {
					a.handleImportTar(w, r, name)
					return
				}
			case "reinstall":
				if method == "POST" && len(parts) == 2 {
					a.handleReinstallServer(w, r, name)
					return
				}
			default:
				if method == "GET" && len(parts) == 2 {
					a.handleServerInfo(w, r, name)
					return
				}
			}
		} else if method == "GET" && len(parts) == 1 {
			name := sanitizeName(parts[0])
			a.handleServerInfo(w, r, name)
			return
		}
		if method == "DELETE" && len(parts) == 1 {
			name := sanitizeName(parts[0])
			a.handleDeleteServer(w, r, name)
			return
		}
	}

	jsonWrite(w, 404, map[string]any{"error": "not found"})
}

func (a *Agent) handlePortCheck(w http.ResponseWriter, r *http.Request) {
	port, ok := parseStrictPortValue(r.URL.Query().Get("port"))
	if !ok {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid port"})
		return
	}
	exclude := sanitizeName(r.URL.Query().Get("exclude"))
	rawProtocols := strings.TrimSpace(r.URL.Query().Get("protocols"))
	if rawProtocols == "" {
		rawProtocols = strings.TrimSpace(r.URL.Query().Get("protocol"))
	}
	protocols := []string{"tcp"}
	if rawProtocols != "" {
		protocols = nil
		for _, part := range strings.Split(rawProtocols, ",") {
			if protocol := normalizePortProtocolName(part); protocol != "" {
				protocols = append(protocols, protocol)
			}
		}
		if len(protocols) == 0 {
			jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid protocol"})
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	available, owner, reason := a.hostPortAvailabilityForProtocols(ctx, port, protocols, exclude)
	cancel()

	payload := map[string]any{
		"ok":        true,
		"port":      port,
		"protocols": protocols,
		"available": available,
	}
	if owner != "" {
		payload["owner"] = owner
	}
	if reason != "" {
		payload["reason"] = reason
	}
	jsonWrite(w, 200, payload)
}

func (a *Agent) handleSystemStats(w http.ResponseWriter, r *http.Request) {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	var ramTotalMb, ramUsedMb, ramFreeMb float64
	if memInfo, err := os.ReadFile("/proc/meminfo"); err == nil {
		lines := strings.Split(string(memInfo), "\n")
		var memTotal, memAvailable, memFree, buffers, cached int64
		for _, line := range lines {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			val, _ := strconv.ParseInt(fields[1], 10, 64)
			switch fields[0] {
			case "MemTotal:":
				memTotal = val
			case "MemAvailable:":
				memAvailable = val
			case "MemFree:":
				memFree = val
			case "Buffers:":
				buffers = val
			case "Cached:":
				cached = val
			}
		}
		ramTotalMb = float64(memTotal) / 1024
		if memAvailable > 0 {
			ramFreeMb = float64(memAvailable) / 1024
		} else {
			ramFreeMb = float64(memFree+buffers+cached) / 1024
		}
		ramUsedMb = ramTotalMb - ramFreeMb
	}

	var cpuPercent float64
	if stat, err := os.ReadFile("/proc/stat"); err == nil {
		lines := strings.Split(string(stat), "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "cpu ") {
				fields := strings.Fields(line)
				if len(fields) >= 5 {
					user, _ := strconv.ParseFloat(fields[1], 64)
					nice, _ := strconv.ParseFloat(fields[2], 64)
					system, _ := strconv.ParseFloat(fields[3], 64)
					idle, _ := strconv.ParseFloat(fields[4], 64)
					iowait := 0.0
					if len(fields) >= 6 {
						iowait, _ = strconv.ParseFloat(fields[5], 64)
					}
					total := user + nice + system + idle + iowait
					if total > 0 {
						cpuPercent = ((total - idle - iowait) / total) * 100
					}
				}
				break
			}
		}
	}

	var diskTotalGb, diskUsedGb, diskFreeGb float64
	if stat, err := os.Stat(a.dataRoot); err == nil && stat.IsDir() {
		var statfs syscall.Statfs_t
		if syscall.Statfs(a.dataRoot, &statfs) == nil {
			diskTotalGb = float64(statfs.Blocks*uint64(statfs.Bsize)) / (1024 * 1024 * 1024)
			diskFreeGb = float64(statfs.Bavail*uint64(statfs.Bsize)) / (1024 * 1024 * 1024)
			diskUsedGb = diskTotalGb - diskFreeGb
		}
	}

	var uptimeSeconds float64
	if uptime, err := os.ReadFile("/proc/uptime"); err == nil {
		fields := strings.Fields(string(uptime))
		if len(fields) >= 1 {
			uptimeSeconds, _ = strconv.ParseFloat(fields[0], 64)
		}
	}

	hostname, _ := os.Hostname()

	jsonWrite(w, 200, map[string]any{
		"ok":            true,
		"cpu_percent":   cpuPercent,
		"cpu_cores":     runtime.NumCPU(),
		"ram_used_mb":   ramUsedMb,
		"ram_total_mb":  ramTotalMb,
		"ram_free_mb":   ramFreeMb,
		"disk_used_gb":  diskUsedGb,
		"disk_total_gb": diskTotalGb,
		"disk_free_gb":  diskFreeGb,
		"uptime":        int64(uptimeSeconds),
		"hostname":      hostname,
		"os":            runtime.GOOS,
		"arch":          runtime.GOARCH,
		"go_version":    runtime.Version(),
		"node_version":  NodeVersion,
	})
}

func (a *Agent) serverDir(name string) string {
	return filepath.Join(a.volumesDir, name)
}

func (a *Agent) metaDir() string {
	return filepath.Join(a.volumesDir, ".adpanel_meta")
}

func (a *Agent) metaPath(name string) string {
	safe := sanitizeName(name)
	if safe == "" {
		safe = "unknown"
	}
	return filepath.Join(a.metaDir(), safe+".json")
}

func (a *Agent) loadMeta(serverDir string) map[string]any {
	serverName := filepath.Base(serverDir)
	extPath := a.metaPath(serverName)
	b, err := os.ReadFile(extPath)
	if err != nil {
		legacyPath := filepath.Join(serverDir, "adpanel.json")
		b, err = os.ReadFile(legacyPath)
		if err != nil {
			return map[string]any{}
		}
		_ = ensureDir(a.metaDir())
		_ = writeFileNoFollow(extPath, b, 0o600)
		_ = os.Remove(legacyPath)
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return map[string]any{}
	}
	return m
}

func (a *Agent) saveMeta(serverDir string, meta map[string]any) error {
	serverName := filepath.Base(serverDir)
	_ = ensureDir(a.metaDir())
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return writeFileNoFollow(a.metaPath(serverName), b, 0o600)
}

func (a *Agent) getMetaLock(serverDir string) *sync.Mutex {
	key := filepath.Clean(serverDir)
	val, _ := a.metaLocks.LoadOrStore(key, &sync.Mutex{})
	return val.(*sync.Mutex)
}

func (a *Agent) modifyMeta(serverDir string, fn func(meta map[string]any)) error {
	mu := a.getMetaLock(serverDir)
	mu.Lock()
	defer mu.Unlock()
	meta := a.loadMeta(serverDir)
	fn(meta)
	return a.saveMeta(serverDir, meta)
}

func (a *Agent) getServerOpLock(name string) *sync.Mutex {
	val, _ := a.serverOpLocks.LoadOrStore(name, &sync.Mutex{})
	return val.(*sync.Mutex)
}

type ServerStatus struct {
	Name     string   `json:"name"`
	Status   string   `json:"status"`
	CPU      *float64 `json:"cpu,omitempty"`
	CpuLimit *float64 `json:"cpuLimit,omitempty"`
	Memory   *float64 `json:"memory,omitempty"`
	Disk     *float64 `json:"disk,omitempty"`
	Uptime   *int64   `json:"uptime,omitempty"`
}

func (a *Agent) handleListServers(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(a.volumesDir)
	if err != nil {
		jsonWrite(w, 500, map[string]any{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var servers []ServerStatus
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		name := e.Name()
		serverDir := a.serverDir(name)

		ss := ServerStatus{
			Name:   name,
			Status: "stopped",
		}

		meta := a.loadMeta(serverDir)
		cpuCoresConfig := metaCpuCores(meta)
		_, cpuLimit := computeCpuLimit(cpuCoresConfig)
		ss.CpuLimit = &cpuLimit

		if a.docker.containerExists(ctx, name) {
			out, _, _, err := a.docker.runCollect(ctx, "inspect", "-f", "{{.State.Running}}", name)
			if err == nil && strings.TrimSpace(out) == "true" {
				ss.Status = "running"

				startedAt, _, _, startErr := a.docker.runCollect(ctx, "inspect", "-f", "{{.State.StartedAt}}", name)
				if startErr == nil && strings.TrimSpace(startedAt) != "" {
					startTime, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(startedAt))
					if parseErr == nil {
						uptime := int64(time.Since(startTime).Seconds())
						ss.Uptime = &uptime
					}
				}

				statsOut, _, _, statsErr := a.docker.runCollect(ctx, "stats", "--no-stream", "--format",
					"{{.CPUPerc}}|{{.MemUsage}}", name)
				if statsErr == nil && strings.TrimSpace(statsOut) != "" {
					parts := strings.Split(strings.TrimSpace(statsOut), "|")
					if len(parts) >= 2 {
						cpuStr := strings.TrimSuffix(strings.TrimSpace(parts[0]), "%")
						if cpu, err := strconv.ParseFloat(cpuStr, 64); err == nil {
							if cpu > cpuLimit {
								cpu = cpuLimit
							}
							ss.CPU = &cpu
						}
						memParts := strings.Split(parts[1], "/")
						if len(memParts) >= 1 {
							memMb := parseMemoryString(strings.TrimSpace(memParts[0]))
							if memMb > 0 {
								ss.Memory = &memMb
							}
						}
					}
				}
			}
		}

		diskBytes := a.getServerDiskUsage(serverDir)
		if diskBytes > 0 {
			diskMb := float64(diskBytes) / (1024 * 1024)
			ss.Disk = &diskMb
		}

		servers = append(servers, ss)
	}

	resources := map[string]any{
		"totalMemory": runtime.MemStats{}.Sys,
		"cpuCount":    runtime.NumCPU(),
	}

	jsonWrite(w, 200, map[string]any{
		"ok":        true,
		"servers":   servers,
		"resources": resources,
	})
}

type resourceLimits struct {
	RamMb                  *int     `json:"ramMb"`
	CpuCores               *float64 `json:"cpuCores"`
	StorageMb              *int     `json:"storageMb"`
	StorageGb              *int     `json:"storageGb"`
	SwapMb                 *int     `json:"swapMb"`
	BackupsMax             *int     `json:"backupsMax"`
	MaxSchedules           *int     `json:"maxSchedules"`
	IoWeight               *int     `json:"ioWeight"`
	CpuWeight              *int     `json:"cpuWeight"`
	PidsLimit              *int     `json:"pidsLimit"`
	FileLimit              *int     `json:"fileLimit"`
	NetworkInboundLimitMb  *int64   `json:"networkInboundLimitMb"`
	NetworkOutboundLimitMb *int64   `json:"networkOutboundLimitMb"`
	NetworkResetMinutes    *int     `json:"networkResetMinutes"`
}

type dockerTemplateConfig struct {
	Image                    string                 `json:"image"`
	Tag                      string                 `json:"tag"`
	Command                  string                 `json:"command"`
	ADPanelStartup           string                 `json:"adpanelStartup"`
	ADPanelStartupSnake      string                 `json:"adpanel_startup"`
	LegacyStartup            string                 `json:"pterodactylStartup"`
	LegacyStartupSnake       string                 `json:"pterodactyl_startup"`
	StartupDisplay           string                 `json:"startupDisplay"`
	StartupDisplaySnake      string                 `json:"startup_display"`
	StartupPreview           string                 `json:"startupPreview"`
	StartupPreviewSnake      string                 `json:"startup_preview"`
	ImageDefaultStartup      string                 `json:"imageDefaultStartup"`
	ImageDefaultStartupSnake string                 `json:"image_default_startup"`
	JavaVersion              string                 `json:"javaVersion"`
	Java                     string                 `json:"java"`
	Ports                    []int                  `json:"ports"`
	PortProtocols            map[string][]string    `json:"portProtocols"`
	Volumes                  []string               `json:"volumes"`
	Workdir                  string                 `json:"workdir"`
	WorkingDir               string                 `json:"workingDir"`
	Env                      map[string]string      `json:"env"`
	Restart                  string                 `json:"restart"`
	RestartPolicy            string                 `json:"restartPolicy"`
	RunAsRoot                bool                   `json:"runAsRoot"`
	RunAsRootSnake           bool                   `json:"run_as_root"`
	StartupCommand           string                 `json:"startupCommand"`
	Install                  *templateInstallConfig `json:"install"`
	Installation             *templateInstallConfig `json:"installation"`
	Console                  any                    `json:"console"`
}

type templateInstallConfig struct {
	Image            string            `json:"image"`
	Container        string            `json:"container"`
	Script           string            `json:"script"`
	Entrypoint       string            `json:"entrypoint"`
	MountPath        string            `json:"mountPath"`
	ContainerPath    string            `json:"containerPath"`
	RuntimePath      string            `json:"runtimePath"`
	TimeoutSeconds   int               `json:"timeoutSeconds"`
	Timeout          int               `json:"timeout"`
	MinimumStorageMb int               `json:"minimumStorageMb"`
	AllowEmpty       bool              `json:"allowEmpty"`
	AllowEmptyResult bool              `json:"allowEmptyResult"`
	Env              map[string]string `json:"env"`
}

type templateStartupCommandReq struct {
	TemplateId      string `json:"templateId"`
	Image           string `json:"image"`
	Tag             string `json:"tag"`
	StartFile       string `json:"startFile"`
	DataDir         string `json:"dataDir"`
	FallbackCommand string `json:"fallbackCommand"`
}

func (a *Agent) handleTemplateStartupCommand(w http.ResponseWriter, r *http.Request) {
	var req templateStartupCommandReq
	if err := readJSON(w, r, 1<<20, &req); err != nil {
		jsonWrite(w, 400, map[string]any{"error": "invalid request body"})
		return
	}

	templateType := strings.ToLower(strings.TrimSpace(req.TemplateId))
	image := strings.TrimSpace(req.Image)
	tag := strings.TrimSpace(req.Tag)
	if image == "" {
		jsonWrite(w, 400, map[string]any{"error": "missing image"})
		return
	}
	if strings.ContainsAny(image, " \t\r\n") {
		jsonWrite(w, 400, map[string]any{"error": "invalid image"})
		return
	}
	if tag == "" {
		tag = "latest"
	}
	if strings.ContainsAny(tag, " \t\r\n") {
		jsonWrite(w, 400, map[string]any{"error": "invalid tag"})
		return
	}

	startFile := strings.TrimSpace(req.StartFile)
	if startFile == "" {
		startFile = defaultStartFileForType(templateType)
	}
	dataDir := strings.TrimSpace(req.DataDir)
	if dataDir == "" {
		dataDir = defaultDataDirForType(templateType)
	}

	imageRef := image + ":" + tag
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	command, source, inspected, err := a.inferRuntimeProcessCommandFromImage(ctx, templateType, imageRef, startFile, dataDir, req.FallbackCommand)
	if err != nil {
		jsonWrite(w, 502, map[string]any{"ok": false, "error": "failed to infer startup command", "detail": err.Error()})
		return
	}
	workingDir := strings.TrimSpace(inspected.Workdir)
	if workingDir == "" {
		workingDir = "/"
	}

	jsonWrite(w, 200, map[string]any{
		"ok":         true,
		"command":    command,
		"source":     source,
		"imageRef":   imageRef,
		"entrypoint": inspected.Entrypoint,
		"cmd":        inspected.Cmd,
		"workingDir": workingDir,
		"env":        inspected.Env,
	})
}

type createReq struct {
	Name           string                `json:"name"`
	Template       string                `json:"templateId"`
	MCFork         string                `json:"mcFork"`
	MCVersion      string                `json:"mcVersion"`
	HostPort       int                   `json:"hostPort"`
	AutoStart      bool                  `json:"autoStart"`
	AsyncSetup     bool                  `json:"asyncSetup"`
	Resources      *resourceLimits       `json:"resources"`
	Docker         *dockerTemplateConfig `json:"docker"`
	StartupCommand string                `json:"startupCommand"`
	JavaVersion    string                `json:"javaVersion"`
	ImportUrl      string                `json:"importUrl"`
}

func (a *Agent) handleCreateServer(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := readJSON(w, r, a.uploadLimit, &req); err != nil {
		jsonWrite(w, 400, map[string]any{"error": "bad json", "detail": err.Error()})
		return
	}

	if a.debug {
		fmt.Printf("[createServer] Received request - Name: %s, Template: %s\n", req.Name, req.Template)
	}

	name := sanitizeName(req.Name)
	if name == "" {
		jsonWrite(w, 400, map[string]any{"error": "invalid name"})
		return
	}
	if strings.TrimSpace(req.Template) == "" {
		jsonWrite(w, 400, map[string]any{"error": "missing templateId"})
		return
	}
	if req.HostPort != 0 {
		if _, ok := parseStrictPortValue(req.HostPort); !ok {
			jsonWrite(w, 400, map[string]any{"error": "invalid hostPort"})
			return
		}
	}
	if req.Docker != nil {
		for _, p := range req.Docker.Ports {
			if _, ok := parseStrictPortValue(p); !ok {
				jsonWrite(w, 400, map[string]any{"error": fmt.Sprintf("invalid docker port: %d", p)})
				return
			}
		}
		if cleanPortProtocols := sanitizeDockerPortProtocols(req.Docker.PortProtocols, req.Docker.Ports); len(req.Docker.PortProtocols) > 0 && len(cleanPortProtocols) == 0 {
			jsonWrite(w, 400, map[string]any{"error": "invalid docker port protocols"})
			return
		}
	}
	if strings.TrimSpace(req.StartupCommand) != "" || (req.Docker != nil && strings.TrimSpace(req.Docker.StartupCommand) != "") {
		jsonWrite(w, 400, map[string]any{"error": "raw docker startup commands are no longer supported"})
		return
	}
	if reason := validateResourcePerformanceLimits(req.Resources); reason != "" {
		jsonWrite(w, 400, map[string]any{"error": reason})
		return
	}

	templateForPort := strings.ToLower(strings.TrimSpace(req.Template))
	hostPortForCheck := req.HostPort
	if hostPortForCheck == 0 {
		if templateForPort == "minecraft" {
			hostPortForCheck = 25565
		} else if req.Docker != nil && len(req.Docker.Ports) > 0 && req.Docker.Ports[0] > 0 {
			hostPortForCheck = req.Docker.Ports[0]
		} else {
			hostPortForCheck = 8080
		}
	}
	protocolsForMainPort := []string{"tcp"}
	if req.Docker != nil {
		portProtocols := runtimePortProtocolMap(sanitizeDockerPortProtocols(req.Docker.PortProtocols, req.Docker.Ports))
		if len(req.Docker.Ports) > 0 {
			protocolsForMainPort = portProtocolsFor(portProtocols, req.Docker.Ports[0])
		}
	}

	if hostPortForCheck > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		available, owner, reason := a.hostPortAvailabilityForProtocols(ctx, hostPortForCheck, protocolsForMainPort, "")
		cancel()
		if !available {
			msg := fmt.Sprintf("port %d is already in use", hostPortForCheck)
			if owner != "" {
				msg = fmt.Sprintf("port %d is already used by container %s", hostPortForCheck, owner)
			} else if reason != "" {
				msg = fmt.Sprintf("port %d is unavailable (%s)", hostPortForCheck, reason)
			}
			jsonWrite(w, 409, map[string]any{"error": msg, "code": "port_in_use", "port": hostPortForCheck, "owner": owner})
			return
		}
	}

	opMu := a.getServerOpLock(name)
	opMu.Lock()
	defer opMu.Unlock()

	serverDir := a.serverDir(name)

	if _, err := os.Stat(serverDir); err == nil {
		extMetaExists := false
		if _, metaErr := os.Stat(a.metaPath(name)); metaErr == nil {
			extMetaExists = true
		}
		legacyPath := filepath.Join(serverDir, "adpanel.json")
		legacyExists := false
		if _, metaErr := os.Stat(legacyPath); metaErr == nil {
			legacyExists = true
		}
		if extMetaExists || legacyExists {
			jsonWrite(w, 400, map[string]any{"error": "server already exists"})
			return
		}
		fmt.Printf("[createServer] Found stale server directory without meta, cleaning up: %s\n", serverDir)
		if err := os.RemoveAll(serverDir); err != nil {
			fmt.Printf("[createServer] Warning: failed to clean stale directory: %v\n", err)
		}
	}
	_ = ensureDir(serverDir)

	template := strings.ToLower(strings.TrimSpace(req.Template))
	meta := map[string]any{}
	if template != "minecraft" {
		if req.Docker == nil || strings.TrimSpace(req.Docker.Image) == "" {
			_ = os.RemoveAll(serverDir)
			jsonWrite(w, 400, map[string]any{"error": "no docker image specified", "detail": "Template payload must include docker.image"})
			return
		}
	}

	resourcesMeta := map[string]any{}
	if req.Resources != nil {
		if req.Resources.RamMb != nil && *req.Resources.RamMb > 0 {
			resourcesMeta["ramMb"] = *req.Resources.RamMb
		}
		if req.Resources.CpuCores != nil && *req.Resources.CpuCores > 0 {
			resourcesMeta["cpuCores"] = *req.Resources.CpuCores
		}
		if req.Resources.StorageMb != nil && *req.Resources.StorageMb > 0 {
			resourcesMeta["storageMb"] = *req.Resources.StorageMb
		} else if req.Resources.StorageGb != nil && *req.Resources.StorageGb > 0 {
			resourcesMeta["storageGb"] = *req.Resources.StorageGb
		}
		if req.Resources.SwapMb != nil {
			resourcesMeta["swapMb"] = *req.Resources.SwapMb
		}
		if req.Resources.BackupsMax != nil {
			resourcesMeta["backupsMax"] = *req.Resources.BackupsMax
		}
		if req.Resources.MaxSchedules != nil {
			resourcesMeta["maxSchedules"] = *req.Resources.MaxSchedules
		}
		if req.Resources.IoWeight != nil {
			resourcesMeta["ioWeight"] = *req.Resources.IoWeight
		}
		if req.Resources.CpuWeight != nil {
			resourcesMeta["cpuWeight"] = *req.Resources.CpuWeight
		}
		if req.Resources.PidsLimit != nil {
			resourcesMeta["pidsLimit"] = *req.Resources.PidsLimit
		}
		if req.Resources.FileLimit != nil {
			resourcesMeta["fileLimit"] = *req.Resources.FileLimit
		}
		if req.Resources.NetworkInboundLimitMb != nil && *req.Resources.NetworkInboundLimitMb > 0 {
			resourcesMeta["networkInboundLimitMb"] = *req.Resources.NetworkInboundLimitMb
		}
		if req.Resources.NetworkOutboundLimitMb != nil && *req.Resources.NetworkOutboundLimitMb > 0 {
			resourcesMeta["networkOutboundLimitMb"] = *req.Resources.NetworkOutboundLimitMb
		}
		if req.Resources.NetworkResetMinutes != nil && *req.Resources.NetworkResetMinutes > 0 {
			resourcesMeta["networkResetMinutes"] = *req.Resources.NetworkResetMinutes
		}
	}

	if template == "minecraft" {
		fork := req.MCFork
		if fork == "" {
			fork = "paper"
		}
		ver := req.MCVersion
		if ver == "" {
			ver = "1.21.8"
		}
		hostPort := req.HostPort
		if hostPort == 0 {
			hostPort = 25565
		}

		writeMinecraftScaffold(serverDir, name, fork, ver)

		mcImage := defaultMinecraftProcessImage
		mcTag := defaultMinecraftProcessTag
		mcCommand := defaultMinecraftProcessCommand
		mcWorkdir := "/data"
		rawJavaVersion := strings.TrimSpace(req.JavaVersion)
		if rawJavaVersion == "" && req.Docker != nil {
			rawJavaVersion = javaVersionFromDockerConfig(req.Docker)
		}
		mcJavaVersion := normalizeMinecraftJavaVersion(rawJavaVersion)
		if strings.TrimSpace(rawJavaVersion) != "" && mcJavaVersion == "" {
			jsonWrite(w, 400, map[string]any{"error": "unsupported minecraft java version"})
			return
		}
		if req.Docker != nil && req.Docker.Image != "" {
			mcImage = req.Docker.Image
			if req.Docker.Tag != "" {
				mcTag = req.Docker.Tag
			}
			reqWorkdir := strings.TrimSpace(req.Docker.Workdir)
			if reqWorkdir == "" {
				reqWorkdir = strings.TrimSpace(req.Docker.WorkingDir)
			}
			if reqWorkdir != "" {
				cleanWorkdir := normalizeContainerWorkdir(reqWorkdir)
				if cleanWorkdir == "" {
					jsonWrite(w, 400, map[string]any{"error": "invalid workdir (must be an absolute path like /data)"})
					return
				}
				mcWorkdir = cleanWorkdir
			}
			if cmd := sanitizeRuntimeCommand(req.Docker.Command); cmd != "" {
				mcCommand = normalizeLegacyRuntimeProcessCommand("minecraft", cmd, "server.jar", "/data")
				if reason := validateRuntimeProcessCommand(mcCommand); reason != "" {
					jsonWrite(w, 400, map[string]any{"error": reason})
					return
				}
			}
		}
		if mcJavaVersion == "" {
			mcJavaVersion = minecraftJavaVersionFromTag(mcTag)
		}
		if mcJavaVersion != "" {
			mcImage = defaultMinecraftProcessImage
			mcTag = minecraftJavaTag(mcJavaVersion)
		}

		meta = map[string]any{
			"type":     "minecraft",
			"fork":     fork,
			"version":  ver,
			"start":    "server.jar",
			"hostPort": hostPort,
			"runtime": map[string]any{
				"image":   mcImage,
				"tag":     mcTag,
				"command": mcCommand,
				"workdir": mcWorkdir,
				"javaVersion": func() any {
					if mcJavaVersion != "" {
						return mcJavaVersion
					}
					return nil
				}(),
			},
		}
		if len(resourcesMeta) > 0 {
			meta["resources"] = resourcesMeta
		}
		if startupDisplay := startupDisplayFromDockerConfig(req.Docker); startupDisplay != "" {
			if runtimeObj, ok := meta["runtime"].(map[string]any); ok {
				runtimeObj["startupDisplay"] = startupDisplay
			}
		}
		if req.Docker != nil {
			if runtimeObj, ok := meta["runtime"].(map[string]any); ok {
				if restart := strings.TrimSpace(req.Docker.Restart); restart != "" {
					runtimeObj["restart"] = restart
				} else if restartPolicy := strings.TrimSpace(req.Docker.RestartPolicy); restartPolicy != "" {
					runtimeObj["restart"] = restartPolicy
				}
				if req.Docker.Console != nil {
					runtimeObj["console"] = req.Docker.Console
				}
			}
		}
		if req.AsyncSetup {
			meta["setupStatus"] = "queued"
			meta["setupMessage"] = "Server setup queued"
			meta["setupUpdatedAt"] = time.Now().UnixMilli()
		}
		_ = a.saveMeta(serverDir, meta)

		if req.AsyncSetup {
			auditLog.Log("server_create", getClientIP(r), "", name, "create", "accepted", map[string]any{"template": req.Template, "asyncSetup": true})
			continueSetup := a.prepareMinecraftCreateSetup(req, name, serverDir, meta)
			if latestMeta := a.loadMeta(serverDir); latestMeta != nil {
				meta = latestMeta
			}
			if continueSetup {
				go a.finishMinecraftCreateSetup(req, name, serverDir, meta, hostPort)
			}
			jsonWrite(w, 200, map[string]any{"ok": true, "name": name, "meta": meta, "asyncSetup": true})
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		jarURL := getMinecraftJarURL(ctx, a.dl, fork, ver, a.debug)
		cancel()

		ctxD, cancelD := context.WithTimeout(context.Background(), 30*time.Minute)
		if err := a.dl.downloadToFile(ctxD, jarURL, filepath.Join(serverDir, "server.jar")); err != nil {
			cancelD()
			jsonWrite(w, 500, map[string]any{"error": "download failed", "detail": err.Error()})
			return
		}
		cancelD()

		enforceServerProps(serverDir)

		if req.ImportUrl != "" {
			fmt.Printf("[createServer] Importing archive from URL for Minecraft server: %s\n", req.ImportUrl)
			if err := a.downloadAndExtractArchiveSync(name, serverDir, req.ImportUrl); err != nil {
				fmt.Printf("[createServer] Warning: archive import failed: %v\n", err)
			}
		}

		if req.AutoStart {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			err := a.startMinecraftContainer(ctx, name, serverDir, hostPort, req.Resources, nil)
			cancel()
			if err != nil {
				fmt.Printf("[createServer] Container startup failed, cleaning up: %s - %v\n", name, err)
				_ = os.RemoveAll(serverDir)
				_ = os.Remove(a.metaPath(name))
				jsonWrite(w, 500, map[string]any{"error": "failed to start server", "detail": err.Error()})
				return
			}
			a.ensureLiveLogs(name)
		}
	} else {
		hostPort := req.HostPort
		if hostPort == 0 {
			if req.Docker != nil && len(req.Docker.Ports) > 0 && req.Docker.Ports[0] > 0 {
				hostPort = req.Docker.Ports[0]
			} else {
				hostPort = 8080
			}
		}

		runtimeConfig := map[string]any{}
		if req.Docker != nil {
			if req.Docker.Image != "" {
				runtimeConfig["image"] = req.Docker.Image
			}
			if req.Docker.Tag != "" {
				runtimeConfig["tag"] = req.Docker.Tag
			}
			if cmd := sanitizeRuntimeCommand(req.Docker.Command); cmd != "" {
				cleanCmd := normalizeLegacyRuntimeProcessCommand(template, cmd, defaultStartFileForType(template), defaultDataDirForType(template))
				if reason := validateRuntimeProcessCommand(cleanCmd); reason != "" {
					jsonWrite(w, 400, map[string]any{"error": reason})
					return
				}
				runtimeConfig["command"] = cleanCmd
			}
			if len(req.Docker.Ports) > 0 {
				runtimeConfig["ports"] = req.Docker.Ports
			}
			if portProtocols := sanitizeDockerPortProtocols(req.Docker.PortProtocols, req.Docker.Ports); len(portProtocols) > 0 {
				runtimeConfig["portProtocols"] = portProtocols
			}
			if len(req.Docker.Volumes) > 0 {
				runtimeConfig["volumes"] = req.Docker.Volumes
			}
			if len(req.Docker.Env) > 0 {
				runtimeConfig["env"] = req.Docker.Env
			}
			if startup := startupTemplateFromDockerConfig(req.Docker); startup != "" {
				runtimeConfig["adpanelStartup"] = startup
				delete(runtimeConfig, "command")
			}
			if startupDisplay := startupDisplayFromDockerConfig(req.Docker); startupDisplay != "" {
				runtimeConfig["startupDisplay"] = startupDisplay
			}
			if install := dockerTemplateInstall(req.Docker); install != nil {
				runtimeConfig["install"] = install
			}
			reqWorkdir := strings.TrimSpace(req.Docker.Workdir)
			if reqWorkdir == "" {
				reqWorkdir = strings.TrimSpace(req.Docker.WorkingDir)
			}
			if reqWorkdir != "" {
				cleanWorkdir := normalizeContainerWorkdir(reqWorkdir)
				if cleanWorkdir == "" {
					jsonWrite(w, 400, map[string]any{"error": "invalid workdir (must be an absolute path like /app)"})
					return
				}
				runtimeConfig["workdir"] = cleanWorkdir
			}
			if restart := strings.TrimSpace(req.Docker.Restart); restart != "" {
				runtimeConfig["restart"] = restart
			} else if restartPolicy := strings.TrimSpace(req.Docker.RestartPolicy); restartPolicy != "" {
				runtimeConfig["restart"] = restartPolicy
			}
			if req.Docker.RunAsRoot || req.Docker.RunAsRootSnake {
				runtimeConfig["runAsRoot"] = true
			}
			if req.Docker.Console != nil {
				runtimeConfig["console"] = req.Docker.Console
			}
			runtimeConfig["providerId"] = "custom"
		}
		if _, ok := runtimeConfig["command"]; !ok {
			if defaultCmd := defaultRuntimeCommandForType(template, defaultStartFileForType(template), defaultDataDirForType(template)); defaultCmd != "" {
				runtimeConfig["command"] = defaultCmd
			}
		}
		if _, ok := runtimeConfig["workdir"]; !ok && hasManagedRuntimeDefaults(template) {
			runtimeConfig["workdir"] = defaultDataDirForType(template)
		}

		meta = map[string]any{
			"type":      template,
			"template":  template,
			"hostPort":  hostPort,
			"createdAt": time.Now().UnixMilli(),
		}
		if len(runtimeConfig) > 0 {
			meta["runtime"] = runtimeConfig
		}
		if len(resourcesMeta) > 0 {
			meta["resources"] = resourcesMeta
		}
		if req.AsyncSetup {
			meta["setupStatus"] = "queued"
			meta["setupMessage"] = "Server setup queued"
			meta["setupUpdatedAt"] = time.Now().UnixMilli()
		}

		fmt.Printf("[createServer] Final meta to save: %+v\n", meta)
		_ = a.saveMeta(serverDir, meta)

		if req.AsyncSetup {
			auditLog.Log("server_create", getClientIP(r), "", name, "create", "accepted", map[string]any{"template": req.Template, "asyncSetup": true})
			continueSetup := a.prepareCustomCreateSetup(req, name, serverDir, meta)
			if latestMeta := a.loadMeta(serverDir); latestMeta != nil {
				meta = latestMeta
			}
			if continueSetup {
				go a.finishCustomCreateSetup(req, name, serverDir, meta, hostPort)
			}
			jsonWrite(w, 200, map[string]any{"ok": true, "name": name, "meta": meta, "asyncSetup": true})
			return
		}

		if install := installConfigFromRuntime(runtimeConfig); install != nil {
			if err := a.runTemplateInstall(name, serverDir, install, runtimeConfig, hostPort, req.Resources); err != nil {
				fmt.Printf("[createServer] Template install failed, cleaning up: %s - %v\n", name, err)
				_ = os.RemoveAll(serverDir)
				_ = os.Remove(a.metaPath(name))
				jsonWrite(w, 500, map[string]any{"error": "failed to install server", "detail": err.Error()})
				return
			}
		}

		if req.ImportUrl != "" {
			fmt.Printf("[createServer] Importing archive from URL: %s\n", req.ImportUrl)
			if err := a.downloadAndExtractArchiveSync(name, serverDir, req.ImportUrl); err != nil {
				fmt.Printf("[createServer] Warning: archive import failed: %v\n", err)
			}
		}
		switch template {
		case "python":
			scaffoldPython(serverDir, defaultStartFileForType(template))
		case "nodejs", "discord-bot":
			scaffoldNode(serverDir, defaultStartFileForType(template), hostPort)
		}

		hasImage := req.Docker != nil && req.Docker.Image != ""
		if req.AutoStart && hasImage {
			ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
			var err error
			if isManagedProcessRuntime(template) {
				err = a.startRuntimeContainer(ctx, name, serverDir, meta, hostPort, req.Resources, nil)
			} else {
				err = a.startCustomDockerContainer(ctx, name, serverDir, meta, hostPort, req.Resources)
			}
			cancel()
			if err != nil {
				fmt.Printf("[createServer] Container startup failed, cleaning up: %s - %v\n", name, err)
				_ = os.RemoveAll(serverDir)
				_ = os.Remove(a.metaPath(name))
				jsonWrite(w, 500, map[string]any{"error": "failed to start server", "detail": err.Error()})
				return
			}
			a.ensureLiveLogs(name)
		}
	}

	auditLog.Log("server_create", getClientIP(r), "", name, "create", "success", map[string]any{"template": req.Template})
	jsonWrite(w, 200, map[string]any{"ok": true, "name": name, "meta": meta})
}

func resourceRamLimitMbFromMeta(meta map[string]any) float64 {
	resources, _ := meta["resources"].(map[string]any)
	if resources == nil {
		return 0
	}
	return anyToFloat64(resources["ramMb"])
}

func resourceStorageLimitMbFromMeta(meta map[string]any) float64 {
	resources, _ := meta["resources"].(map[string]any)
	if resources == nil {
		return 0
	}
	if v := anyToFloat64(resources["storageMb"]); v > 0 {
		return v
	}
	if v := anyToFloat64(resources["storageGb"]); v > 0 {
		return v * 1024
	}
	return 0
}

func buildNetworkStatsPayload(inboundBytes, outboundBytes uint64) map[string]any {
	return map[string]any{
		"inboundBytes":  inboundBytes,
		"outboundBytes": outboundBytes,
		"rxBytes":       inboundBytes,
		"txBytes":       outboundBytes,
	}
}

type networkAntiAbuseRule struct {
	inboundLimitBytes  uint64
	outboundLimitBytes uint64
	resetEvery         time.Duration
	enabled            bool
}

func networkLimitMbToBytes(mb int64) uint64 {
	if mb <= 0 {
		return 0
	}
	maxMb := int64(^uint64(0) / (1024 * 1024))
	if mb > maxMb {
		return ^uint64(0)
	}
	return uint64(mb) * 1024 * 1024
}

func networkAntiAbuseRuleFromMeta(meta map[string]any) networkAntiAbuseRule {
	resources, _ := meta["resources"].(map[string]any)
	if resources == nil {
		return networkAntiAbuseRule{}
	}
	inboundMb := anyToInt64(resources["networkInboundLimitMb"])
	outboundMb := anyToInt64(resources["networkOutboundLimitMb"])
	if inboundMb <= 0 && outboundMb <= 0 {
		return networkAntiAbuseRule{}
	}
	resetMinutes := anyToInt(resources["networkResetMinutes"])
	if resetMinutes <= 0 {
		resetMinutes = defaultNetworkResetMins
	}
	if resetMinutes < minNetworkResetMins || resetMinutes > maxNetworkResetMins {
		resetMinutes = defaultNetworkResetMins
	}
	return networkAntiAbuseRule{
		inboundLimitBytes:  networkLimitMbToBytes(inboundMb),
		outboundLimitBytes: networkLimitMbToBytes(outboundMb),
		resetEvery:         time.Duration(resetMinutes) * time.Minute,
		enabled:            inboundMb > 0 || outboundMb > 0,
	}
}

func uint64Remaining(limit, used uint64) uint64 {
	if limit == 0 || used >= limit {
		return 0
	}
	return limit - used
}

func attachNetworkAntiAbusePayload(payload map[string]any, rule networkAntiAbuseRule, windowRx, windowTx uint64, windowStartedAt time.Time) {
	if payload == nil || !rule.enabled {
		return
	}
	payload["windowInboundBytes"] = windowRx
	payload["windowOutboundBytes"] = windowTx
	payload["inboundLimitBytes"] = rule.inboundLimitBytes
	payload["outboundLimitBytes"] = rule.outboundLimitBytes
	payload["inboundRemainingBytes"] = uint64Remaining(rule.inboundLimitBytes, windowRx)
	payload["outboundRemainingBytes"] = uint64Remaining(rule.outboundLimitBytes, windowTx)
	payload["resetEveryMinutes"] = int(rule.resetEvery / time.Minute)
	if !windowStartedAt.IsZero() && rule.resetEvery > 0 {
		payload["windowStartedAt"] = windowStartedAt.UTC().Format(time.RFC3339)
		payload["windowResetsAt"] = windowStartedAt.Add(rule.resetEvery).UTC().Format(time.RFC3339)
	}
}

func normalizeDockerContainerStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "running":
		return "running"
	case "created", "exited", "dead", "removing", "paused":
		return "stopped"
	case "":
		return "stopped"
	default:
		return strings.ToLower(strings.TrimSpace(status))
	}
}

func (a *Agent) getServerResourcePayload(ctx context.Context, name string) (map[string]any, error) {
	serverDir := a.serverDir(name)
	if st, err := os.Stat(serverDir); err != nil || !st.IsDir() {
		return nil, os.ErrNotExist
	}

	if a.resourceStats == nil {
		return a.collectServerResourcePayload(ctx, name, serverDir, nil), nil
	}

	entry := a.resourceStats.get(name)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	now := time.Now()
	entry.touchedAt = now
	if entry.payload != nil && now.Before(entry.expiresAt) {
		return entry.payload, nil
	}

	payload := a.collectServerResourcePayload(ctx, name, serverDir, entry)
	entry.payload = payload
	entry.expiresAt = now.Add(a.resourceStats.ttl)
	return payload, nil
}

func (a *Agent) collectServerResourcePayload(ctx context.Context, name, serverDir string, entry *serverResourceStatsCacheEntry) map[string]any {
	meta := a.loadMeta(serverDir)
	status := "stopped"
	now := time.Now()

	memoryUsedMb := float64(0)
	memoryLimitMb := resourceRamLimitMbFromMeta(meta)
	memoryPercent := float64(0)
	uptimeSeconds := int64(0)

	cpuEffCores, cpuMaxPercent := computeCpuLimit(metaCpuCores(meta))
	cpuPercent := float64(0)

	networkRule := networkAntiAbuseRuleFromMeta(meta)
	var networkPayload map[string]any
	if entry != nil && entry.seenNetwork {
		networkPayload = buildNetworkStatsPayload(entry.totalRx, entry.totalTx)
		attachNetworkAntiAbusePayload(networkPayload, networkRule, entry.networkWindowRx, entry.networkWindowTx, entry.networkWindowStartedAt)
	}

	inspected, exists, inspectErr := a.inspectContainerForStats(ctx, name)
	if inspectErr != nil {
		status = "unknown"
	} else if exists {
		status = normalizeDockerContainerStatus(inspected.State.Status)
		if inspected.State.Running {
			status = "running"

			if appliedMemoryMb := hostConfigMemoryLimitMb(inspected.HostConfig); appliedMemoryMb > 0 {
				memoryLimitMb = appliedMemoryMb
			}
			if appliedCPULimit := hostConfigCpuLimitPercent(inspected.HostConfig); appliedCPULimit > 0 {
				cpuMaxPercent = appliedCPULimit
				cpuEffCores = appliedCPULimit / 100.0
			}

			if strings.TrimSpace(inspected.State.StartedAt) != "" {
				if startTime, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(inspected.State.StartedAt)); err == nil {
					uptimeSeconds = int64(time.Since(startTime).Seconds())
				}
			}

			if stats, err := a.readContainerStatsForServer(ctx, name); err == nil {
				cpuPercent = dockerCPUPercent(stats)
				if cpuMaxPercent > 0 && cpuPercent > cpuMaxPercent {
					cpuPercent = cpuMaxPercent
				}

				memoryUsedMb = float64(dockerMemoryUsedBytes(stats.MemoryStats)) / (1024 * 1024)
				if memoryLimitMb > 0 {
					memoryPercent = (memoryUsedMb / memoryLimitMb) * 100
				}

				if len(stats.Networks) > 0 && entry != nil {
					rawRx, rawTx := sumDockerNetworkBytes(stats.Networks)
					totalRx, totalTx, windowRx, windowTx, windowStartedAt := entry.updateNetworkCounters(inspected.ID, rawRx, rawTx, now, networkRule.resetEvery)
					networkPayload = buildNetworkStatsPayload(totalRx, totalTx)
					attachNetworkAntiAbusePayload(networkPayload, networkRule, windowRx, windowTx, windowStartedAt)
				}
			}
		}
	}

	var diskUsageBytes int64
	if entry != nil {
		diskUsageBytes = entry.cachedDiskUsage(a, serverDir, now, a.resourceStats.diskTTL)
	} else {
		diskUsageBytes = a.getServerDiskUsage(serverDir)
	}
	diskUsageMb := float64(diskUsageBytes) / (1024 * 1024)
	diskUsageGb := float64(diskUsageBytes) / (1024 * 1024 * 1024)

	storageLimitMb := resourceStorageLimitMbFromMeta(meta)
	diskInfo := map[string]any{
		"usedBytes": diskUsageBytes,
		"usedMb":    diskUsageMb,
		"usedGb":    diskUsageGb,
	}
	diskPercent := float64(0)
	if storageLimitMb > 0 {
		limitBytes := int64(storageLimitMb) * 1024 * 1024
		diskInfo["limitBytes"] = limitBytes
		diskInfo["limitGb"] = storageLimitMb / 1024
		diskInfo["limitMb"] = storageLimitMb
		if limitBytes > 0 {
			diskPercent = (float64(diskUsageBytes) / float64(limitBytes)) * 100
		}
		diskInfo["percentUsed"] = diskPercent
	}

	stats := map[string]any{
		"cpu": map[string]any{
			"percent": cpuPercent,
			"cores":   cpuEffCores,
			"limit":   cpuMaxPercent,
		},
		"memory": map[string]any{
			"usedMb":  memoryUsedMb,
			"limitMb": memoryLimitMb,
			"percent": memoryPercent,
		},
		"disk": map[string]any{
			"usedMb":  diskUsageMb,
			"limitMb": storageLimitMb,
			"percent": diskPercent,
		},
	}
	if networkPayload != nil {
		stats["network"] = networkPayload
	}

	payload := map[string]any{
		"ok":     true,
		"name":   name,
		"status": status,
		"meta":   meta,
		"disk":   diskInfo,
		"stats":  stats,
		"uptime": uptimeSeconds,
	}
	if networkPayload != nil {
		payload["network"] = networkPayload
	}
	return payload
}

func (a *Agent) handleServerInfo(w http.ResponseWriter, r *http.Request, name string) {
	ctx, cancel := context.WithTimeout(r.Context(), 3500*time.Millisecond)
	defer cancel()

	payload, err := a.getServerResourcePayload(ctx, name)
	if errors.Is(err, os.ErrNotExist) {
		jsonWrite(w, 404, map[string]any{"error": "not found"})
		return
	}
	if err != nil {
		jsonWrite(w, 500, map[string]any{"error": "failed to collect resource stats"})
		return
	}
	jsonWrite(w, 200, payload)
}

func (a *Agent) handleGetResources(w http.ResponseWriter, r *http.Request, name string) {
	serverDir := a.serverDir(name)
	if st, err := os.Stat(serverDir); err != nil || !st.IsDir() {
		jsonWrite(w, 404, map[string]any{"error": "not found"})
		return
	}

	meta := a.loadMeta(serverDir)
	resources, _ := meta["resources"].(map[string]any)
	if resources == nil {
		resources = make(map[string]any)
	}

	if _, hasMb := resources["storageMb"]; !hasMb {
		if gb, hasGb := resources["storageGb"]; hasGb {
			var gbVal float64
			switch v := gb.(type) {
			case float64:
				gbVal = v
			case int:
				gbVal = float64(v)
			case int64:
				gbVal = float64(v)
			}
			if gbVal > 0 {
				resources["storageMb"] = int(gbVal * 1024)
			}
		}
	}
	configuredCommand := ""
	command := ""
	commandSource := ""
	imageDefaultCommand := ""
	imageRef := ""
	templateType := ""
	javaVersion := ""
	if t, ok := meta["template"].(string); ok {
		templateType = t
	} else if t, ok := meta["type"].(string); ok {
		templateType = t
	}
	startFile := strings.TrimSpace(fmt.Sprint(meta["start"]))
	runtimeObj, _ := meta["runtime"].(map[string]any)
	if runtimeObj != nil {
		configuredCommand = normalizeLegacyRuntimeProcessCommand(templateType, fmt.Sprint(runtimeObj["command"]), startFile, defaultDataDirForType(templateType))
		adpanelStartup := startupTemplateFromRuntime(runtimeObj)
		if adpanelStartup != "" {
			configuredCommand = ""
			command = adpanelStartup
			commandSource = "adpanel-startup"
		}
		image := strings.TrimSpace(fmt.Sprint(runtimeObj["image"]))
		tag := strings.TrimSpace(fmt.Sprint(runtimeObj["tag"]))
		if image != "" && image != "<nil>" {
			if tag == "" || tag == "<nil>" {
				tag = "latest"
			}
			imageRef = image + ":" + tag
			javaVersion = normalizeMinecraftJavaVersion(runtimeObj["javaVersion"])
			if javaVersion == "" {
				javaVersion = minecraftJavaVersionFromTag(tag)
			}
		}
	}
	if command == "" && configuredCommand != "" {
		command = configuredCommand
		commandSource = "configured"
	}
	if command == "" {
		command = defaultRuntimeCommandForType(
			templateType,
			startFile,
			defaultDataDirForType(templateType),
		)
		if command != "" {
			commandSource = "template-default"
		}
	}
	if command == "" && imageRef != "" {
		inspectCtx, inspectCancel := context.WithTimeout(context.Background(), 8*time.Second)
		imgCfg := a.inspectRuntimeImageConfig(inspectCtx, imageRef)
		inspectCancel()
		imageDefaultCommand = normalizeLegacyRuntimeProcessCommand(
			templateType,
			runtimeCommandFromImageConfig(imgCfg),
			startFile,
			defaultDataDirForType(templateType),
		)
		if imageDefaultCommand != "" && validateRuntimeProcessCommand(imageDefaultCommand) == "" {
			command = imageDefaultCommand
			commandSource = "image-default"
		}
	}
	if command == "" && imageRef != "" {
		command = "sleep infinity"
		commandSource = "fallback"
	}

	template := templateType

	var memoryUsedMb, memoryLimitMb float64
	if res, ok := resources["ramMb"]; ok {
		switch v := res.(type) {
		case float64:
			memoryLimitMb = v
		case int:
			memoryLimitMb = float64(v)
		case int64:
			memoryLimitMb = float64(v)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if a.docker.containerExists(ctx, name) {
		out, _, _, err := a.docker.runCollect(ctx, "inspect", "-f", "{{.State.Running}}", name)
		if err == nil && strings.TrimSpace(out) == "true" {
			if inspectedMemoryLimitMb, _, inspectErr := a.inspectContainerAppliedLimits(ctx, name); inspectErr == nil && inspectedMemoryLimitMb > 0 {
				memoryLimitMb = inspectedMemoryLimitMb
			}
			statsOut, _, _, statsErr := a.docker.runCollect(ctx, "stats", "--no-stream", "--format",
				"{{.MemUsage}}", name)
			if statsErr == nil && strings.TrimSpace(statsOut) != "" {
				memParts := strings.Split(strings.TrimSpace(statsOut), "/")
				if len(memParts) >= 2 {
					memoryUsedMb = parseMemoryString(strings.TrimSpace(memParts[0]))
				}
			}
		}
	}

	stats := map[string]any{
		"memory": map[string]any{
			"usedMb":  memoryUsedMb,
			"limitMb": memoryLimitMb,
		},
	}

	var hostPort int
	if port, ok := parseStrictPortValue(meta["hostPort"]); ok {
		hostPort = port
	}

	payload := map[string]any{
		"ok":                  true,
		"resources":           resources,
		"command":             command,
		"configuredCommand":   configuredCommand,
		"effectiveCommand":    command,
		"commandSource":       commandSource,
		"imageDefaultCommand": imageDefaultCommand,
		"imageRef":            imageRef,
		"template":            template,
		"stats":               stats,
		"hostPort":            hostPort,
	}
	if strings.EqualFold(strings.TrimSpace(templateType), "minecraft") {
		payload["javaVersion"] = javaVersion
		payload["minecraftJava"] = minecraftJavaOptionsPayload(javaVersion)
	}
	jsonWrite(w, 200, payload)
}

func (a *Agent) buildEffectiveDockerCommand(name, serverDir string, meta map[string]any) string {
	typ := strings.ToLower(fmt.Sprint(meta["type"]))
	runtimeObj, _ := meta["runtime"].(map[string]any)
	if runtimeObj == nil {
		runtimeObj = make(map[string]any)
	}

	hostPort := 0
	if port, ok := parseStrictPortValue(meta["hostPort"]); ok {
		hostPort = port
	}

	resources := resourceLimitsFromMeta(meta)
	appendPerformancePreviewArgs := func(parts []string) []string {
		if resources == nil {
			return parts
		}
		if resources.PidsLimit != nil && *resources.PidsLimit >= minPidsLimit && *resources.PidsLimit <= maxPidsLimit {
			parts = append(parts, "--pids-limit", fmt.Sprintf("%d", *resources.PidsLimit))
		}
		if resources.CpuWeight != nil && *resources.CpuWeight >= minCpuWeight && *resources.CpuWeight <= maxCpuWeight {
			parts = append(parts, "--cpu-shares", fmt.Sprintf("%d", dockerCpuSharesFromWeight(*resources.CpuWeight)))
		}
		if resources.IoWeight != nil && *resources.IoWeight >= minIoWeight && *resources.IoWeight <= maxIoWeight {
			parts = append(parts, "--blkio-weight", fmt.Sprintf("%d", *resources.IoWeight))
		}
		if resources.FileLimit != nil && *resources.FileLimit >= minFileLimit && *resources.FileLimit <= maxFileLimit {
			parts = append(parts, "--ulimit", fmt.Sprintf("nofile=%d:%d", *resources.FileLimit, *resources.FileLimit))
		}
		return parts
	}

	rm, _ := meta["resources"].(map[string]any)
	var ramMb, cpuCoresVal, swapMb float64
	var hasRam, hasCpu, hasSwap bool
	if rm != nil {
		if v, ok := rm["ramMb"].(float64); ok && v > 0 {
			ramMb = v
			hasRam = true
		}
		if v, ok := rm["cpuCores"].(float64); ok && v > 0 {
			cpuCoresVal = v
			hasCpu = true
		}
		if v, ok := rm["swapMb"].(float64); ok {
			swapMb = v
			hasSwap = true
		}
	}

	switch typ {
	case "minecraft":
		if hostPort == 0 {
			hostPort = 25565
		}
		uid, gid := statUIDGID(serverDir)
		parts := []string{dockerRunInteractivePrefix(),
			"--name", name,
			"--restart", "unless-stopped",
			"-p", fmt.Sprintf("%d:25565", hostPort),
		}
		portForwardsMC := a.loadPortForwards(serverDir)
		for _, pf := range portForwardsMC {
			parts = append(parts, "-p", fmt.Sprintf("%d:%d", pf.Public, 25565))
		}
		parts = append(parts,
			"-v", fmt.Sprintf("%s:/data", serverDir),
			"-e", "EULA=TRUE",
			"-e", "TYPE=CUSTOM",
			"-e", "CUSTOM_SERVER=/data/server.jar",
			"-e", "ENABLE_RCON=false",
			"-e", "CREATE_CONSOLE_IN_PIPE=true",
			"-e", fmt.Sprintf("UID=%d", uid),
			"-e", fmt.Sprintf("GID=%d", gid),
		)
		if hasRam {
			parts = append(parts, "--memory", fmt.Sprintf("%dm", int(ramMb)))
			parts = append(parts, "--memory-reservation", fmt.Sprintf("%dm", int(ramMb*90/100)))
			if !hasSwap || int(swapMb) == -1 {
				parts = append(parts, "--memory-swap", fmt.Sprintf("%dm", int(ramMb)))
			} else if int(swapMb) == 0 {
				parts = append(parts, "--memory-swap", "-1")
			} else {
				parts = append(parts, "--memory-swap", fmt.Sprintf("%dm", int(ramMb)+int(swapMb)))
			}
		}
		if hasCpu && cpuCoresVal <= 128 {
			cpuQuota := int64(float64(cpuPeriod) * cpuCoresVal)
			parts = append(parts, "--cpu-period", fmt.Sprintf("%d", cpuPeriod))
			parts = append(parts, "--cpu-quota", fmt.Sprintf("%d", cpuQuota))
		}
		parts = appendPerformancePreviewArgs(parts)
		mcImage := strings.TrimSpace(fmt.Sprint(runtimeObj["image"]))
		mcTag := strings.TrimSpace(fmt.Sprint(runtimeObj["tag"]))
		if mcImage == "" || mcImage == "<nil>" {
			mcImage = "itzg/minecraft-server"
		}
		if mcTag == "" || mcTag == "<nil>" {
			mcTag = "latest"
		}
		parts = append(parts, mcImage+":"+mcTag)
		return strings.Join(parts, " ")

	case "python", "nodejs", "discord-bot", "runtime":
		isPython := typ == "python"
		isNode := typ == "nodejs" || typ == "discord-bot"
		image := strings.TrimSpace(fmt.Sprint(runtimeObj["image"]))
		tag := strings.TrimSpace(fmt.Sprint(runtimeObj["tag"]))
		if image == "" || image == "<nil>" {
			if isPython {
				image = "python"
			} else {
				image = "node"
			}
		}
		if tag == "" || tag == "<nil>" {
			if isPython {
				tag = "3.12-slim"
			} else {
				tag = "20-alpine"
			}
		}
		imageRef := image + ":" + tag

		parts := []string{dockerRunInteractivePrefix(), "--name", name, "--restart", "unless-stopped"}
		if hasRam {
			parts = append(parts, "--memory", fmt.Sprintf("%dm", int(ramMb)))
			parts = append(parts, "--memory-reservation", fmt.Sprintf("%dm", int(ramMb*90/100)))
			if !hasSwap || int(swapMb) == -1 {
				parts = append(parts, "--memory-swap", fmt.Sprintf("%dm", int(ramMb)))
			} else if int(swapMb) == 0 {
				parts = append(parts, "--memory-swap", "-1")
			} else {
				parts = append(parts, "--memory-swap", fmt.Sprintf("%dm", int(ramMb)+int(swapMb)))
			}
		}
		if hasCpu && cpuCoresVal <= 128 {
			cpuQuota := int64(float64(cpuPeriod) * cpuCoresVal)
			parts = append(parts, "--cpu-period", fmt.Sprintf("%d", cpuPeriod))
			parts = append(parts, "--cpu-quota", fmt.Sprintf("%d", cpuQuota))
		}
		parts = appendPerformancePreviewArgs(parts)
		if hostPort > 0 {
			parts = append(parts, "-p", fmt.Sprintf("%d:%d", hostPort, hostPort))
		}
		portForwardsRT := a.loadPortForwards(serverDir)
		for _, pf := range portForwardsRT {
			parts = append(parts, "-p", fmt.Sprintf("%d:%d", pf.Public, pf.Internal))
		}
		vols := []string{fmt.Sprintf("%s:/app", serverDir)}
		if runtimeVolumes := runtimeStringSlice(runtimeObj["volumes"]); len(runtimeVolumes) > 0 {
			vols = vols[:0]
			for _, s := range runtimeVolumes {
				if s != "" {
					vols = append(vols, s)
				}
			}
			if len(vols) == 0 {
				vols = []string{fmt.Sprintf("%s:/app", serverDir)}
			}
		}
		for _, v := range vols {
			parts = append(parts, "-v", v)
		}
		if env := runtimeEnvMap(runtimeObj["env"]); len(env) > 0 {
			for k, v := range env {
				parts = append(parts, "-e", fmt.Sprintf("%s=%s", k, v))
			}
		}
		if hostPort > 0 {
			parts = append(parts, "-e", fmt.Sprintf("PORT=%d", hostPort))
		}
		uid, gid := statUIDGID(serverDir)
		parts = append(parts, "-e", fmt.Sprintf("UID=%d", uid))
		parts = append(parts, "-e", fmt.Sprintf("GID=%d", gid))
		parts = append(parts, imageRef)

		startFile := strings.TrimSpace(fmt.Sprint(meta["start"]))
		if startFile == "" || startFile == "<nil>" {
			if isPython {
				startFile = "main.py"
			} else {
				startFile = "index.js"
			}
		}
		cmd := strings.TrimSpace(fmt.Sprint(runtimeObj["command"]))
		if cmd == "" || cmd == "<nil>" {
			if isPython {
				cmd = fmt.Sprintf("python /app/%s", startFile)
			} else if isNode {
				cmd = fmt.Sprintf("node /app/%s", startFile)
			}
		}
		if cmd != "" {
			cmd = normalizeLegacyRuntimeProcessCommand(typ, cmd, startFile, "/app")
			if args, err := buildProcessCommandArgs(cmd); err == nil {
				parts = append(parts, args...)
			}
		}
		return strings.Join(parts, " ")

	default:
		image := strings.TrimSpace(fmt.Sprint(runtimeObj["image"]))
		tag := strings.TrimSpace(fmt.Sprint(runtimeObj["tag"]))
		if image == "" || image == "<nil>" {
			return ""
		}
		if tag == "" || tag == "<nil>" {
			tag = "latest"
		}
		imageRef := image + ":" + tag
		if hostPort == 0 {
			hostPort = 8080
		}
		parts := []string{dockerRunInteractivePrefix(), "--name", name, "--restart", "unless-stopped"}
		if hasRam {
			parts = append(parts, "--memory", fmt.Sprintf("%dm", int(ramMb)))
			parts = append(parts, "--memory-reservation", fmt.Sprintf("%dm", int(ramMb*90/100)))
		}
		if hasCpu && cpuCoresVal <= 128 {
			cpuQuota := int64(float64(cpuPeriod) * cpuCoresVal)
			parts = append(parts, "--cpu-period", fmt.Sprintf("%d", cpuPeriod))
			parts = append(parts, "--cpu-quota", fmt.Sprintf("%d", cpuQuota))
		}
		parts = appendPerformancePreviewArgs(parts)
		defaultContainerPort := hostPort
		if ports := runtimeIntSlice(runtimeObj["ports"]); len(ports) > 0 && ports[0] > 0 {
			defaultContainerPort = ports[0]
		}
		parts = append(parts, "-p", fmt.Sprintf("%d:%d", hostPort, defaultContainerPort))
		portForwards := a.loadPortForwards(serverDir)
		for _, pf := range portForwards {
			containerPort := defaultContainerPort
			if containerPort <= 0 {
				containerPort = pf.Internal
			}
			parts = append(parts, "-p", fmt.Sprintf("%d:%d", pf.Public, containerPort))
		}
		vols := []string{}
		hasTemplateVols := false
		if runtimeVolumes := runtimeStringSlice(runtimeObj["volumes"]); len(runtimeVolumes) > 0 {
			for _, s := range runtimeVolumes {
				if s != "" {
					s = strings.ReplaceAll(s, "{BOT_DIR}", serverDir)
					s = strings.ReplaceAll(s, "{SERVER_DIR}", serverDir)
					vols = append(vols, s)
					hasTemplateVols = true
				}
			}
		}
		if !hasTemplateVols {
			inspCtx, inspCancel := context.WithTimeout(context.Background(), 10*time.Second)
			imgCfg := a.docker.inspectImageConfig(inspCtx, imageRef)
			inspCancel()
			if len(imgCfg.Volumes) > 0 {
				for _, p := range imgCfg.Volumes {
					vols = append(vols, fmt.Sprintf("%s:%s", serverDir, p))
				}
			} else if envPaths := getDataPathsFromEnv(imgCfg.Env); len(envPaths) > 0 {
				vols = append(vols, fmt.Sprintf("%s:%s", serverDir, envPaths[0]))
			} else if imgCfg.Workdir != "" && imgCfg.Workdir != "/" {
				vols = append(vols, fmt.Sprintf("%s:%s", serverDir, imgCfg.Workdir))
			} else {
				vols = append(vols, fmt.Sprintf("%s:/data", serverDir))
			}
		}
		for _, v := range vols {
			parts = append(parts, "-v", v)
		}
		if env := runtimeEnvMap(runtimeObj["env"]); len(env) > 0 {
			for k, v := range env {
				parts = append(parts, "-e", fmt.Sprintf("%s=%s", k, v))
			}
		}
		if hostPort > 0 {
			parts = append(parts, "-e", fmt.Sprintf("PORT=%d", hostPort))
		}
		parts = append(parts, imageRef)
		cmd := strings.TrimSpace(fmt.Sprint(runtimeObj["command"]))
		if cmd != "" && cmd != "<nil>" {
			parts = append(parts, "sh", "-lc", fmt.Sprintf("%q", cmd))
		}
		return strings.Join(parts, " ")
	}
}

func (a *Agent) handleUpdateResources(w http.ResponseWriter, r *http.Request, name string) {
	serverDir := a.serverDir(name)
	if st, err := os.Stat(serverDir); err != nil || !st.IsDir() {
		jsonWrite(w, 404, map[string]any{"error": "not found"})
		return
	}

	var req struct {
		Resources struct {
			RamMb                  *int     `json:"ramMb"`
			CpuCores               *float64 `json:"cpuCores"`
			StorageMb              *int     `json:"storageMb"`
			StorageGb              *int     `json:"storageGb"`
			SwapMb                 *int     `json:"swapMb"`
			BackupsMax             *int     `json:"backupsMax"`
			MaxSchedules           *int     `json:"maxSchedules"`
			IoWeight               *int     `json:"ioWeight"`
			CpuWeight              *int     `json:"cpuWeight"`
			PidsLimit              *int     `json:"pidsLimit"`
			FileLimit              *int     `json:"fileLimit"`
			NetworkInboundLimitMb  *int64   `json:"networkInboundLimitMb"`
			NetworkOutboundLimitMb *int64   `json:"networkOutboundLimitMb"`
			NetworkResetMinutes    *int     `json:"networkResetMinutes"`
			Ports                  []int    `json:"ports"`
			Command                *string  `json:"command"`
			StartupCommand         *string  `json:"startupCommand"`
			JavaVersion            *string  `json:"javaVersion"`
		} `json:"resources"`
		Command        *string `json:"command"`
		MainPort       *int    `json:"mainPort"`
		StartupCommand *string `json:"startupCommand"`
		JavaVersion    *string `json:"javaVersion"`
	}

	if err := readJSON(w, r, 1<<20, &req); err != nil {
		jsonWrite(w, 400, map[string]any{"error": "invalid request body"})
		return
	}
	if (req.StartupCommand != nil && strings.TrimSpace(*req.StartupCommand) != "") ||
		(req.Resources.StartupCommand != nil && strings.TrimSpace(*req.Resources.StartupCommand) != "") {
		jsonWrite(w, 400, map[string]any{"error": "raw docker startup commands are no longer supported"})
		return
	}
	if req.MainPort != nil {
		if _, ok := parseStrictPortValue(*req.MainPort); !ok {
			jsonWrite(w, 400, map[string]any{"error": "invalid mainPort"})
			return
		}
	}
	if reason := validateResourcePerformanceLimits(&resourceLimits{
		IoWeight:               req.Resources.IoWeight,
		CpuWeight:              req.Resources.CpuWeight,
		PidsLimit:              req.Resources.PidsLimit,
		FileLimit:              req.Resources.FileLimit,
		NetworkInboundLimitMb:  req.Resources.NetworkInboundLimitMb,
		NetworkOutboundLimitMb: req.Resources.NetworkOutboundLimitMb,
		NetworkResetMinutes:    req.Resources.NetworkResetMinutes,
	}); reason != "" {
		jsonWrite(w, 400, map[string]any{"error": reason})
		return
	}

	metaMu := a.getMetaLock(serverDir)
	metaMu.Lock()
	defer metaMu.Unlock()

	meta := a.loadMeta(serverDir)
	if meta == nil {
		meta = make(map[string]any)
	}
	stripLegacyStartupCommandFields(meta)

	resources, ok := meta["resources"].(map[string]any)
	if !ok {
		resources = make(map[string]any)
	}

	if req.Resources.RamMb != nil {
		resources["ramMb"] = *req.Resources.RamMb
	}
	if req.Resources.CpuCores != nil {
		resources["cpuCores"] = *req.Resources.CpuCores
	}
	if req.Resources.StorageMb != nil {
		resources["storageMb"] = *req.Resources.StorageMb
		delete(resources, "storageGb")
	} else if req.Resources.StorageGb != nil {
		resources["storageGb"] = *req.Resources.StorageGb
		delete(resources, "storageMb")
	}
	if req.Resources.SwapMb != nil {
		resources["swapMb"] = *req.Resources.SwapMb
	}
	if req.Resources.BackupsMax != nil {
		resources["backupsMax"] = *req.Resources.BackupsMax
	}
	if req.Resources.MaxSchedules != nil {
		resources["maxSchedules"] = *req.Resources.MaxSchedules
	}
	if req.Resources.IoWeight != nil {
		resources["ioWeight"] = *req.Resources.IoWeight
	}
	if req.Resources.CpuWeight != nil {
		resources["cpuWeight"] = *req.Resources.CpuWeight
	}
	if req.Resources.PidsLimit != nil {
		resources["pidsLimit"] = *req.Resources.PidsLimit
	}
	if req.Resources.FileLimit != nil {
		resources["fileLimit"] = *req.Resources.FileLimit
	}
	if req.Resources.NetworkInboundLimitMb != nil {
		if *req.Resources.NetworkInboundLimitMb > 0 {
			resources["networkInboundLimitMb"] = *req.Resources.NetworkInboundLimitMb
		} else {
			delete(resources, "networkInboundLimitMb")
		}
	}
	if req.Resources.NetworkOutboundLimitMb != nil {
		if *req.Resources.NetworkOutboundLimitMb > 0 {
			resources["networkOutboundLimitMb"] = *req.Resources.NetworkOutboundLimitMb
		} else {
			delete(resources, "networkOutboundLimitMb")
		}
	}
	if req.Resources.NetworkResetMinutes != nil {
		if *req.Resources.NetworkResetMinutes > 0 {
			resources["networkResetMinutes"] = *req.Resources.NetworkResetMinutes
		} else {
			delete(resources, "networkResetMinutes")
		}
	}
	if req.Resources.Ports != nil {
		validPorts := make([]int, 0, len(req.Resources.Ports))
		seenPorts := make(map[int]bool, len(req.Resources.Ports))
		for _, p := range req.Resources.Ports {
			if _, ok := parseStrictPortValue(p); !ok {
				jsonWrite(w, 400, map[string]any{"error": fmt.Sprintf("invalid port: %d", p)})
				return
			}
			if seenPorts[p] {
				continue
			}
			seenPorts[p] = true
			validPorts = append(validPorts, p)
		}
		resources["ports"] = validPorts
	}

	meta["resources"] = resources
	if req.MainPort != nil {
		meta["hostPort"] = *req.MainPort
	}

	javaVersionProvided := req.JavaVersion != nil || req.Resources.JavaVersion != nil
	if javaVersionProvided {
		rawJavaVersion := ""
		if req.JavaVersion != nil {
			rawJavaVersion = *req.JavaVersion
		} else if req.Resources.JavaVersion != nil {
			rawJavaVersion = *req.Resources.JavaVersion
		}
		cleanJavaVersion := normalizeMinecraftJavaVersion(rawJavaVersion)
		if cleanJavaVersion == "" {
			jsonWrite(w, 400, map[string]any{"error": "unsupported minecraft java version"})
			return
		}
		templateType := strings.ToLower(strings.TrimSpace(fmt.Sprint(meta["template"])))
		if templateType == "" {
			templateType = strings.ToLower(strings.TrimSpace(fmt.Sprint(meta["type"])))
		}
		runtimeObj, _ := meta["runtime"].(map[string]any)
		if runtimeObj == nil {
			runtimeObj = make(map[string]any)
		}
		commandLooksMinecraft := strings.Contains(strings.ToLower(sanitizeRuntimeCommand(runtimeObj["command"])), "server.jar")
		if templateType != "minecraft" && !strings.Contains(templateType, "minecraft") && !commandLooksMinecraft {
			jsonWrite(w, 400, map[string]any{"error": "java version selection is only available for minecraft"})
			return
		}
		runtimeObj["image"] = defaultMinecraftProcessImage
		runtimeObj["tag"] = minecraftJavaTag(cleanJavaVersion)
		runtimeObj["javaVersion"] = cleanJavaVersion
		if sanitizeRuntimeCommand(runtimeObj["command"]) == "" {
			runtimeObj["command"] = defaultMinecraftProcessCommand
		}
		if normalizeContainerWorkdir(runtimeObj["workdir"]) == "" {
			runtimeObj["workdir"] = defaultDataDirForType("minecraft")
		}
		meta["runtime"] = runtimeObj
	}

	commandProvided := req.Command != nil || req.Resources.Command != nil
	if commandProvided {
		rawCommand := ""
		if req.Command != nil {
			rawCommand = *req.Command
		} else if req.Resources.Command != nil {
			rawCommand = *req.Resources.Command
		}
		runtimeObj, _ := meta["runtime"].(map[string]any)
		if runtimeObj == nil {
			runtimeObj = make(map[string]any)
		}
		templateType := strings.ToLower(strings.TrimSpace(fmt.Sprint(meta["template"])))
		if templateType == "" {
			templateType = strings.ToLower(strings.TrimSpace(fmt.Sprint(meta["type"])))
		}
		startFile := strings.TrimSpace(fmt.Sprint(meta["start"]))
		cleanCommand := normalizeLegacyRuntimeProcessCommand(templateType, rawCommand, startFile, defaultDataDirForType(templateType))
		if cleanCommand != "" {
			if reason := validateRuntimeProcessCommand(cleanCommand); reason != "" {
				jsonWrite(w, 400, map[string]any{"error": reason})
				return
			}
		}
		if cleanCommand == "" {
			delete(runtimeObj, "command")
		} else {
			runtimeObj["command"] = cleanCommand
		}
		meta["runtime"] = runtimeObj
	}

	if err := a.saveMeta(serverDir, meta); err != nil {
		jsonWrite(w, 500, map[string]any{"error": "failed to save meta"})
		return
	}

	if a.debug {
		a.logf("[resources] Updated resources for %s: %+v", name, resources)
	}

	jsonWrite(w, 200, map[string]any{
		"ok":        true,
		"resources": resources,
		"command": func() string {
			if runtimeObj, ok := meta["runtime"].(map[string]any); ok {
				return sanitizeRuntimeCommand(runtimeObj["command"])
			}
			return ""
		}(),
		"javaVersion": func() string {
			if runtimeObj, ok := meta["runtime"].(map[string]any); ok {
				return normalizeMinecraftJavaVersion(runtimeObj["javaVersion"])
			}
			return ""
		}(),
		"hostPort": meta["hostPort"],
	})
}

func (a *Agent) handleGetPortForwards(w http.ResponseWriter, r *http.Request, name string) {
	serverDir := a.serverDir(name)
	if st, err := os.Stat(serverDir); err != nil || !st.IsDir() {
		jsonWrite(w, 404, map[string]any{"error": "not found"})
		return
	}
	meta := a.loadMeta(serverDir)
	portForwards := []map[string]any{}
	if pf, ok := meta["portForwards"].([]any); ok {
		for _, item := range pf {
			if m, ok := item.(map[string]any); ok {
				pub, pubOk := parseStrictPortValue(m["publicPort"])
				internal, internalOk := parseStrictPortValue(m["internalPort"])
				if pubOk && internalOk {
					portForwards = append(portForwards, map[string]any{
						"publicPort":   pub,
						"internalPort": internal,
					})
				}
			}
		}
	}

	hostPort := 0
	if port, ok := parseStrictPortValue(meta["hostPort"]); ok {
		hostPort = port
	}
	allocatedPorts := []int{}
	if hostPort > 0 {
		allocatedPorts = append(allocatedPorts, hostPort)
	}
	if resources, ok := meta["resources"].(map[string]any); ok {
		for _, port := range runtimeIntSlice(resources["ports"]) {
			if port > 0 {
				allocatedPorts = append(allocatedPorts, port)
			}
		}
	}

	jsonWrite(w, 200, map[string]any{
		"ok":             true,
		"portForwards":   portForwards,
		"hostPort":       meta["hostPort"],
		"allocatedPorts": allocatedPorts,
	})
}

func (a *Agent) handleUpdatePortForwards(w http.ResponseWriter, r *http.Request, name string) {
	serverDir := a.serverDir(name)
	if st, err := os.Stat(serverDir); err != nil || !st.IsDir() {
		jsonWrite(w, 404, map[string]any{"error": "not found"})
		return
	}

	var req struct {
		PortForwards []struct {
			PublicPort   int `json:"publicPort"`
			InternalPort int `json:"internalPort"`
		} `json:"portForwards"`
	}
	if err := readJSON(w, r, 1<<20, &req); err != nil {
		jsonWrite(w, 400, map[string]any{"error": "invalid request body"})
		return
	}

	metaMu := a.getMetaLock(serverDir)
	metaMu.Lock()
	defer metaMu.Unlock()

	meta := a.loadMeta(serverDir)
	if meta == nil {
		meta = make(map[string]any)
	}

	allocatedSet := make(map[int]bool)
	hostPort := 0
	if port, ok := parseStrictPortValue(meta["hostPort"]); ok {
		hostPort = port
	}
	if hostPort > 0 {
		allocatedSet[hostPort] = true
	}
	if resources, ok := meta["resources"].(map[string]any); ok {
		for _, port := range runtimeIntSlice(resources["ports"]) {
			if port > 0 {
				allocatedSet[port] = true
			}
		}
	}

	forwards := make([]map[string]any, 0, len(req.PortForwards))
	seen := make(map[int]bool)
	for _, pf := range req.PortForwards {
		if pf.PublicPort < 1 || pf.PublicPort > 65535 {
			jsonWrite(w, 400, map[string]any{"error": fmt.Sprintf("invalid public port: %d", pf.PublicPort)})
			return
		}
		if pf.InternalPort < 1 || pf.InternalPort > 65535 {
			jsonWrite(w, 400, map[string]any{"error": fmt.Sprintf("invalid internal port: %d", pf.InternalPort)})
			return
		}
		if !allocatedSet[pf.InternalPort] {
			jsonWrite(w, 400, map[string]any{"error": fmt.Sprintf("internal port %d is not allocated to this server", pf.InternalPort)})
			return
		}
		if seen[pf.PublicPort] {
			jsonWrite(w, 400, map[string]any{"error": fmt.Sprintf("duplicate public port: %d", pf.PublicPort)})
			return
		}
		seen[pf.PublicPort] = true
		forwards = append(forwards, map[string]any{
			"publicPort":   pf.PublicPort,
			"internalPort": pf.InternalPort,
		})
	}

	meta["portForwards"] = forwards
	if err := a.saveMeta(serverDir, meta); err != nil {
		jsonWrite(w, 500, map[string]any{"error": "failed to save meta"})
		return
	}

	fmt.Printf("[port-forwards] Updated port forwards for %s: %d rules\n", name, len(forwards))
	jsonWrite(w, 200, map[string]any{"ok": true})
}

func (a *Agent) loadPortForwards(serverDir string) []struct{ Public, Internal int } {
	meta := a.loadMeta(serverDir)
	var result []struct{ Public, Internal int }
	pf, ok := meta["portForwards"].([]any)
	if !ok {
		return result
	}
	for _, item := range pf {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		pub, pubOk := parseStrictPortValue(m["publicPort"])
		internal, internalOk := parseStrictPortValue(m["internalPort"])
		if pubOk && internalOk {
			result = append(result, struct{ Public, Internal int }{pub, internal})
		}
	}
	return result
}

func (a *Agent) handleExtractImage(w http.ResponseWriter, r *http.Request, name string) {
	serverDir := a.serverDir(name)
	if st, err := os.Stat(serverDir); err != nil || !st.IsDir() {
		jsonWrite(w, 404, map[string]any{"error": "not found"})
		return
	}
	if diskErr := a.checkDiskLimit(serverDir, 0); diskErr != nil {
		jsonWrite(w, 507, diskErr)
		return
	}

	var req struct {
		Image       string   `json:"image"`
		Tag         string   `json:"tag"`
		ExtractPath string   `json:"extractPath"`
		Paths       []string `json:"paths"`
	}

	if err := readJSON(w, r, 1<<20, &req); err != nil {
		jsonWrite(w, 400, map[string]any{"error": "invalid request body"})
		return
	}

	image := strings.TrimSpace(req.Image)
	tag := strings.TrimSpace(req.Tag)

	if image == "" {
		meta := a.loadMeta(serverDir)
		if meta != nil {
			if rt, ok := meta["runtime"].(map[string]any); ok {
				image = strings.TrimSpace(fmt.Sprint(rt["image"]))
				if tag == "" {
					tag = strings.TrimSpace(fmt.Sprint(rt["tag"]))
				}
			}
		}
	}

	if image == "" {
		jsonWrite(w, 400, map[string]any{"error": "no image specified"})
		return
	}

	if tag == "" {
		tag = "latest"
	}
	imageRef := image + ":" + tag

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	fmt.Printf("[extract-image] Pulling image %s for server %s\n", imageRef, name)
	a.docker.pull(ctx, imageRef)

	extractPaths := req.Paths
	if len(extractPaths) == 0 && req.ExtractPath != "" {
		extractPaths = []string{req.ExtractPath}
	}
	if len(extractPaths) == 0 {
		extractPaths = []string{
			"/ragemp", "/fivem", "/config", "/server", "/gameserver",
			"/rust", "/ark", "/minecraft", "/csgo", "/valheim",
			"/app", "/data", "/opt", "/home",
			"/usr/src/app", "/usr/app", "/var/www", "/srv",
		}
	}

	var extractedPath string
	for _, extractPath := range extractPaths {
		err := a.docker.extractImageFiles(ctx, imageRef, serverDir, extractPath)
		if err == nil {
			extractedPath = extractPath
			fmt.Printf("[extract-image] Extracted files from %s:%s to %s\n", imageRef, extractPath, serverDir)
			break
		}
	}

	if extractedPath == "" {
		jsonWrite(w, 200, map[string]any{
			"ok":        true,
			"extracted": false,
			"message":   "Could not extract files - image may not have files in common locations",
		})
		return
	}

	jsonWrite(w, 200, map[string]any{
		"ok":        true,
		"extracted": true,
		"path":      extractedPath,
		"image":     imageRef,
	})
}

func (a *Agent) handleImportArchive(w http.ResponseWriter, r *http.Request, name string) {
	srvDir := a.serverDir(name)

	if st, err := os.Stat(srvDir); err != nil || !st.IsDir() {
		jsonWrite(w, 404, map[string]any{"error": "server not found", "detail": "Server directory does not exist"})
		return
	}
	if diskErr := a.checkDiskLimit(srvDir, 0); diskErr != nil {
		jsonWrite(w, 507, diskErr)
		return
	}

	var req struct {
		ArchiveUrl string `json:"archiveUrl"`
	}

	if err := readJSON(w, r, 1<<20, &req); err != nil {
		jsonWrite(w, 400, map[string]any{"error": "invalid request body"})
		return
	}

	archiveUrl := strings.TrimSpace(req.ArchiveUrl)
	if archiveUrl == "" {
		jsonWrite(w, 400, map[string]any{"error": "archiveUrl is required"})
		return
	}

	parsedUrl, err := url.Parse(archiveUrl)
	if err != nil || (parsedUrl.Scheme != "http" && parsedUrl.Scheme != "https") {
		jsonWrite(w, 400, map[string]any{"error": "invalid URL - must be http or https"})
		return
	}

	pathname := strings.ToLower(parsedUrl.Path)
	validExt := false
	allowedExts := []string{".zip", ".tar.gz", ".tgz", ".tar.bz2", ".tbz2", ".tar.xz", ".txz", ".7z", ".rar"}
	for _, ext := range allowedExts {
		if strings.HasSuffix(pathname, ext) {
			validExt = true
			break
		}
	}
	if !validExt {
		jsonWrite(w, 400, map[string]any{"error": "invalid archive format. Supported: .zip, .tar.gz, .tgz, .tar.bz2, .tbz2, .7z, .rar"})
		return
	}

	a.importProgressMu.Lock()
	a.importProgress[name] = &ImportProgress{
		Status:     "downloading",
		Percent:    0,
		Downloaded: 0,
		Total:      0,
	}
	a.importProgressMu.Unlock()

	go a.downloadAndExtractArchive(name, srvDir, archiveUrl)

	jsonWrite(w, 200, map[string]any{"ok": true, "message": "Import started"})
}

func (a *Agent) handleExportServer(w http.ResponseWriter, r *http.Request, name string) {
	serverDir := a.serverDir(name)
	if st, err := os.Stat(serverDir); err != nil || !st.IsDir() {
		jsonWrite(w, 404, map[string]any{"error": "server not found"})
		return
	}

	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	backupsDir := a.serverBackupsDir(name)

	sizeBytes, _ := getDirSizeBytes(serverDir)
	if st, err := os.Stat(backupsDir); err == nil && st.IsDir() {
		if bsz, err := getDirSizeBytes(backupsDir); err == nil {
			sizeBytes += bsz
		}
	}
	if sizeBytes > 0 {
		w.Header().Set("X-ADPanel-Server-Bytes", fmt.Sprint(sizeBytes))
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s.tar", name))

	tw := tar.NewWriter(w)
	defer tw.Close()

	err := filepath.Walk(serverDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		rel, relErr := filepath.Rel(serverDir, p)
		if relErr != nil {
			return nil
		}
		if rel == "." {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}

		hdr, hdrErr := tar.FileInfoHeader(info, "")
		if hdrErr != nil {
			return nil
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() && !strings.HasSuffix(hdr.Name, "/") {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, openErr := os.Open(p)
		if openErr != nil {
			return nil
		}
		_, _ = io.Copy(tw, f)
		f.Close()
		return nil
	})
	if err != nil {
		jsonWrite(w, 500, map[string]any{"error": "failed to walk directory"})
		return
	}

	if st, err := os.Stat(backupsDir); err == nil && st.IsDir() {
		err := filepath.Walk(backupsDir, func(p string, info os.FileInfo, err error) error {
			if err != nil || info == nil {
				return nil
			}
			rel, relErr := filepath.Rel(backupsDir, p)
			if relErr != nil {
				return nil
			}
			if rel == "." {
				return nil
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return nil
			}

			hdr, hdrErr := tar.FileInfoHeader(info, "")
			if hdrErr != nil {
				return nil
			}
			hdr.Name = ".adpanel_backups/" + filepath.ToSlash(rel)
			if info.IsDir() && !strings.HasSuffix(hdr.Name, "/") {
				hdr.Name += "/"
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			f, openErr := os.Open(p)
			if openErr != nil {
				return nil
			}
			_, _ = io.Copy(tw, f)
			f.Close()
			return nil
		})
		if err != nil {
			jsonWrite(w, 500, map[string]any{"error": "failed to walk directory"})
			return
		}
	}

	metaPath := a.metaPath(name)
	if metaInfo, metaErr := os.Stat(metaPath); metaErr == nil && metaInfo.Mode().IsRegular() {
		hdr, hdrErr := tar.FileInfoHeader(metaInfo, "")
		if hdrErr == nil {
			hdr.Name = ".adpanel_meta/" + sanitizeName(name) + ".json"
			if err := tw.WriteHeader(hdr); err == nil {
				if f, openErr := os.Open(metaPath); openErr == nil {
					_, _ = io.Copy(tw, f)
					f.Close()
				}
			}
		}
	}
}

func (a *Agent) handleImportTar(w http.ResponseWriter, r *http.Request, name string) {
	if name == "" {
		jsonWrite(w, 400, map[string]any{"error": "invalid name"})
		return
	}

	serverDir := a.serverDir(name)
	if diskErr := a.checkDiskLimit(serverDir, 0); diskErr != nil {
		jsonWrite(w, 507, diskErr)
		return
	}
	_ = os.RemoveAll(serverDir)
	_ = ensureDir(serverDir)

	backupsDir := a.serverBackupsDir(name)
	_ = os.RemoveAll(backupsDir)
	_ = ensureDir(backupsDir)

	_ = ensureDir(a.metaDir())
	tr := tar.NewReader(r.Body)
	if err := extractTarSafeRouted(tr, serverDir, backupsDir, a.metaDir()); err != nil {
		jsonWrite(w, 500, map[string]any{"ok": false, "error": "extract failed", "detail": err.Error()})
		return
	}
	jsonWrite(w, 200, map[string]any{"ok": true})
}

func (a *Agent) downloadAndExtractArchive(name, serverDir, archiveUrl string) {
	a.importSem <- struct{}{}
	defer func() { <-a.importSem }()

	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "[import-archive] panic recovered for %s: %v\n", name, r)
			a.importProgressMu.Lock()
			a.importProgress[name] = &ImportProgress{
				Status: "error", Error: "internal error",
			}
			a.importProgressMu.Unlock()
		}
		time.AfterFunc(5*time.Minute, func() {
			a.importProgressMu.Lock()
			delete(a.importProgress, name)
			a.importProgressMu.Unlock()
		})
	}()

	updateProgress := func(status string, percent int, downloaded, total int64, errMsg string) {
		a.importProgressMu.Lock()
		a.importProgress[name] = &ImportProgress{
			Status:     status,
			Percent:    percent,
			Downloaded: downloaded,
			Total:      total,
			Error:      errMsg,
		}
		a.importProgressMu.Unlock()
	}

	if _, err := a.dl.safeURL(archiveUrl); err != nil {
		updateProgress("error", 0, 0, 0, fmt.Sprintf("invalid URL: blocked by security policy"))
		if a.debug {
			fmt.Printf("[import-archive] SSRF blocked for %s: %v\n", name, err)
		}
		return
	}

	if a.debug {
		fmt.Printf("[import-archive] Starting download for server %s from %s\n", name, archiveUrl)
	}

	dlCtx, dlCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer dlCancel()
	req, err := http.NewRequestWithContext(dlCtx, "GET", archiveUrl, nil)
	if err != nil {
		if a.debug {
			fmt.Printf("[import-archive] Request creation error for %s: %v\n", name, err)
		}
		updateProgress("error", 0, 0, 0, "download failed")
		return
	}
	resp, err := a.dl.client().Do(req)
	if err != nil {
		if a.debug {
			fmt.Printf("[import-archive] Download error for %s: %v\n", name, err)
		}
		updateProgress("error", 0, 0, 0, "download failed")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		if a.debug {
			fmt.Printf("[import-archive] HTTP error for %s: %d\n", name, resp.StatusCode)
		}
		updateProgress("error", 0, 0, 0, fmt.Sprintf("HTTP error: %d", resp.StatusCode))
		return
	}

	totalSize := resp.ContentLength
	if totalSize <= 0 {
		totalSize = 0
	}

	tmpFile, err := os.CreateTemp("", "import-archive-*")
	if err != nil {
		updateProgress("error", 0, 0, 0, "failed to create temp file")
		return
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	var downloaded int64 = 0
	buf := make([]byte, 32*1024)

	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			_, writeErr := tmpFile.Write(buf[:n])
			if writeErr != nil {
				tmpFile.Close()
				updateProgress("error", 0, downloaded, totalSize, "failed to write temp file")
				return
			}
			downloaded += int64(n)

			var percent int
			if totalSize > 0 {
				percent = int((downloaded * 100) / totalSize)
			} else {
				percent = 0
			}
			updateProgress("downloading", percent, downloaded, totalSize, "")
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			tmpFile.Close()
			updateProgress("error", 0, downloaded, totalSize, fmt.Sprintf("download error: %v", readErr))
			return
		}
	}
	tmpFile.Close()

	fmt.Printf("[import-archive] Download complete for %s: %d bytes\n", name, downloaded)

	updateProgress("extracting", 100, downloaded, totalSize, "")

	pathname := strings.ToLower(archiveUrl)
	var extractErr error

	if strings.HasSuffix(pathname, ".zip") {
		extractErr = extractZipSafe(tmpPath, serverDir)
	} else if strings.HasSuffix(pathname, ".tar.gz") || strings.HasSuffix(pathname, ".tgz") {
		extractErr = a.extractTarGzSafe(tmpPath, serverDir)
	} else if strings.HasSuffix(pathname, ".tar.bz2") || strings.HasSuffix(pathname, ".tbz2") {
		extractErr = a.extractTarBz2Safe(tmpPath, serverDir)
	} else if strings.HasSuffix(pathname, ".tar.xz") || strings.HasSuffix(pathname, ".txz") {
		extractErr = a.extractTarXzSafe(tmpPath, serverDir)
	} else if strings.HasSuffix(pathname, ".7z") {
		extractErr = a.extract7z(tmpPath, serverDir)
	} else if strings.HasSuffix(pathname, ".rar") {
		extractErr = a.extractRar(tmpPath, serverDir)
	} else {
		extractErr = fmt.Errorf("unsupported archive format")
	}

	if extractErr != nil {
		if a.debug {
			fmt.Printf("[import-archive] Extract error for %s: %v\n", name, extractErr)
		}
		updateProgress("error", 100, downloaded, totalSize, fmt.Sprintf("extraction failed: %v", extractErr))
		return
	}

	_ = sanitizeExtractedTree(serverDir)

	if a.debug {
		fmt.Printf("[import-archive] Extraction complete for %s\n", name)
	}
	updateProgress("complete", 100, downloaded, totalSize, "")
}

func (a *Agent) extractZip(zipPath, destDir string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		fpath := filepath.Join(destDir, f.Name)
		if !strings.HasPrefix(filepath.Clean(fpath), filepath.Clean(destDir)+string(os.PathSeparator)) {
			continue
		}

		if f.FileInfo().IsDir() {
			os.MkdirAll(fpath, 0o755)
			continue
		}

		if err := os.MkdirAll(filepath.Dir(fpath), 0o755); err != nil {
			return err
		}

		outFile, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, sanitizeFileMode(f.Mode(), false))
		if err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			outFile.Close()
			return err
		}

		_, err = io.Copy(outFile, rc)
		rc.Close()
		outFile.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) extractTarGz(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gzr.Close()

	return a.extractTar(gzr, destDir)
}

func (a *Agent) extractTarBz2(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	bzr := bzip2.NewReader(f)
	return a.extractTar(bzr, destDir)
}

func (a *Agent) extractTarGzSafe(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	return extractTarSafe(tr, destDir)
}

func (a *Agent) extractTarBz2Safe(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	bzr := bzip2.NewReader(f)
	tr := tar.NewReader(bzr)
	return extractTarSafe(tr, destDir)
}

func (a *Agent) extractTarXzSafe(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	xzr, err := xz.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(xzr)
	return extractTarSafe(tr, destDir)
}

func (a *Agent) extractTar(r io.Reader, destDir string) error {
	tr := tar.NewReader(r)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		fpath := filepath.Join(destDir, header.Name)
		if !strings.HasPrefix(filepath.Clean(fpath), filepath.Clean(destDir)+string(os.PathSeparator)) {
			continue
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(fpath, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(fpath), 0o755); err != nil {
				return err
			}
			outFile, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, sanitizeFileMode(os.FileMode(header.Mode), false))
			if err != nil {
				return err
			}
			if _, err := io.Copy(outFile, tr); err != nil {
				outFile.Close()
				return err
			}
			outFile.Close()
		}
	}
	return nil
}

func (a *Agent) extract7z(archivePath, destDir string) error {
	tmpDir, err := os.MkdirTemp("", "adpanel-7z-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cmd := exec.Command("7z", "x", "-y", "-o"+tmpDir, archivePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("7z extraction failed: %v - %s", err, string(output))
	}

	return validateAndMoveExtracted(tmpDir, destDir)
}

func (a *Agent) extractRar(archivePath, destDir string) error {
	tmpDir, err := os.MkdirTemp("", "adpanel-rar-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cmd := exec.Command("unrar", "x", "-y", archivePath, tmpDir+"/")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("rar extraction failed: %v - %s", err, string(output))
	}

	return validateAndMoveExtracted(tmpDir, destDir)
}

func validateAndMoveExtracted(tmpDir, destDir string) error {
	return filepath.Walk(tmpDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(tmpDir, path)
		if err != nil {
			return fmt.Errorf("cannot compute relative path: %v", err)
		}
		if rel == "." {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		dest := filepath.Join(destDir, rel)
		if !isUnder(destDir, dest) {
			return fmt.Errorf("zip-slip blocked: %s escapes destination", rel)
		}
		if info.IsDir() {
			return os.MkdirAll(dest, sanitizeFileMode(info.Mode(), true))
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if moveErr := os.Rename(path, dest); moveErr != nil {
			return moveErr
		}
		return os.Chmod(dest, sanitizeFileMode(info.Mode(), false))
	})
}

func (a *Agent) downloadAndExtractArchiveSync(name, serverDir, archiveUrl string) error {
	if _, err := a.dl.safeURL(archiveUrl); err != nil {
		if a.debug {
			fmt.Printf("[import-archive] SSRF blocked for %s: %v\n", name, err)
		}
		return fmt.Errorf("invalid URL: blocked by security policy")
	}

	parsedUrl, _ := url.Parse(archiveUrl)
	pathname := strings.ToLower(parsedUrl.Path)
	validExt := false
	allowedExts := []string{".zip", ".tar.gz", ".tgz", ".tar.bz2", ".tbz2", ".tar.xz", ".txz", ".7z", ".rar"}
	for _, ext := range allowedExts {
		if strings.HasSuffix(pathname, ext) {
			validExt = true
			break
		}
	}
	if !validExt {
		return fmt.Errorf("unsupported archive format (supported: .zip, .tar.gz, .tgz, .tar.bz2, .tbz2, .tar.xz, .txz, .7z, .rar)")
	}

	if a.debug {
		fmt.Printf("[import-archive] Starting download for server %s from %s\n", name, archiveUrl)
	}

	resp, err := a.dl.client().Get(archiveUrl)
	if err != nil {
		return fmt.Errorf("download failed")
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP error: %d", resp.StatusCode)
	}

	const maxImportSize int64 = 10 << 30
	if resp.ContentLength > maxImportSize {
		return fmt.Errorf("archive too large (max 10GB)")
	}

	tmpFile, err := os.CreateTemp("", "import-archive-*")
	if err != nil {
		return fmt.Errorf("failed to create temp file")
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	limitedReader := io.LimitReader(resp.Body, maxImportSize)
	downloaded, err := io.Copy(tmpFile, limitedReader)
	tmpFile.Close()
	if err != nil {
		return fmt.Errorf("download error")
	}

	if a.debug {
		fmt.Printf("[import-archive] Download complete for %s: %d bytes\n", name, downloaded)
	}

	archivePathLower := strings.ToLower(archiveUrl)
	var extractErr error

	if strings.HasSuffix(archivePathLower, ".zip") {
		extractErr = extractZipSafe(tmpPath, serverDir)
	} else if strings.HasSuffix(archivePathLower, ".tar.gz") || strings.HasSuffix(archivePathLower, ".tgz") {
		extractErr = a.extractTarGzSafe(tmpPath, serverDir)
	} else if strings.HasSuffix(archivePathLower, ".tar.bz2") || strings.HasSuffix(archivePathLower, ".tbz2") {
		extractErr = a.extractTarBz2Safe(tmpPath, serverDir)
	} else if strings.HasSuffix(archivePathLower, ".tar.xz") || strings.HasSuffix(archivePathLower, ".txz") {
		extractErr = a.extractTarXzSafe(tmpPath, serverDir)
	} else if strings.HasSuffix(archivePathLower, ".7z") {
		extractErr = a.extract7z(tmpPath, serverDir)
	} else if strings.HasSuffix(archivePathLower, ".rar") {
		extractErr = a.extractRar(tmpPath, serverDir)
	} else {
		extractErr = fmt.Errorf("unsupported archive format")
	}

	if extractErr != nil {
		return fmt.Errorf("extraction failed: %v", extractErr)
	}

	_ = sanitizeExtractedTree(serverDir)

	if a.debug {
		fmt.Printf("[import-archive] Extraction complete for %s\n", name)
	}
	return nil
}

func (a *Agent) handleImportArchiveProgress(w http.ResponseWriter, r *http.Request, name string) {
	a.importProgressMu.RLock()
	progress, exists := a.importProgress[name]
	a.importProgressMu.RUnlock()

	if !exists {
		jsonWrite(w, 200, map[string]any{
			"status":     "not_found",
			"percent":    0,
			"downloaded": 0,
			"total":      0,
		})
		return
	}

	jsonWrite(w, 200, map[string]any{
		"status":     progress.Status,
		"percent":    progress.Percent,
		"downloaded": progress.Downloaded,
		"total":      progress.Total,
		"error":      progress.Error,
	})
}

func parseMemoryString(s string) float64 {
	s = strings.TrimSpace(s)
	s = strings.ToUpper(s)

	var multiplier float64 = 1
	var numStr string

	if strings.HasSuffix(s, "GIB") {
		multiplier = 1024
		numStr = strings.TrimSuffix(s, "GIB")
	} else if strings.HasSuffix(s, "GB") {
		multiplier = 1024
		numStr = strings.TrimSuffix(s, "GB")
	} else if strings.HasSuffix(s, "MIB") {
		multiplier = 1
		numStr = strings.TrimSuffix(s, "MIB")
	} else if strings.HasSuffix(s, "MB") {
		multiplier = 1
		numStr = strings.TrimSuffix(s, "MB")
	} else if strings.HasSuffix(s, "KIB") {
		multiplier = 1.0 / 1024
		numStr = strings.TrimSuffix(s, "KIB")
	} else if strings.HasSuffix(s, "KB") {
		multiplier = 1.0 / 1024
		numStr = strings.TrimSuffix(s, "KB")
	} else {
		numStr = s
	}

	val, err := strconv.ParseFloat(strings.TrimSpace(numStr), 64)
	if err != nil {
		return 0
	}
	return val * multiplier
}

type applyReq struct {
	URL        string `json:"url"`
	NodeID     string `json:"nodeId"`
	DestPath   string `json:"destPath"`
	ProviderID string `json:"providerId"`
	VersionID  string `json:"versionId"`
}

func (a *Agent) handleApplyVersion(w http.ResponseWriter, r *http.Request, name string) {
	var req applyReq
	if err := readJSON(w, r, 1<<20, &req); err != nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "bad-json", "detail": err.Error()})
		return
	}
	if name == "" || strings.TrimSpace(req.URL) == "" {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "missing-fields"})
		return
	}
	serverDir := a.serverDir(name)
	if diskErr := a.checkDiskLimit(serverDir, 0); diskErr != nil {
		diskErr["ok"] = false
		jsonWrite(w, 507, diskErr)
		return
	}
	if !strings.HasPrefix(strings.ToLower(req.URL), "http://") && !strings.HasPrefix(strings.ToLower(req.URL), "https://") {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid-url"})
		return
	}
	if req.NodeID != "" && a.uuid != "" && req.NodeID != a.uuid {
		jsonWrite(w, 401, map[string]any{"ok": false, "error": "unauthorized"})
		return
	}
	ok, why := a.verifyPanelSignature(r, name, req.ProviderID, req.VersionID, req.URL, nil)
	if !ok {
		jsonWrite(w, 403, map[string]any{"ok": false, "error": why})
		return
	}

	destRel := strings.TrimSpace(req.DestPath)
	if destRel == "" {
		destRel = "server.jar"
	}

	baseDir := a.serverDir(name)
	st, err := os.Stat(baseDir)
	if err != nil || !st.IsDir() {
		jsonWrite(w, 404, map[string]any{"ok": false, "error": "server-dir-not-found"})
		return
	}

	target, pathOk := safeJoin(baseDir, destRel)
	if !pathOk {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid-destPath"})
		return
	}
	_ = ensureDir(filepath.Dir(target))

	if destRel == "server.jar" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		a.docker.stop(ctx, name)
		cancel()

		ctxD, cancelD := context.WithTimeout(context.Background(), 30*time.Minute)
		if err := a.dl.downloadToFile(ctxD, req.URL, target); err != nil {
			cancelD()
			jsonWrite(w, 500, map[string]any{"ok": false, "error": "download failed", "detail": err.Error()})
			return
		}
		cancelD()

		hostPort := 25565
		meta := a.loadMeta(baseDir)
		if v, ok := meta["hostPort"]; ok {
			switch vv := v.(type) {
			case float64:
				hostPort = int(vv)
			case int:
				hostPort = vv
			}
		}
		enforceServerProps(baseDir)

		ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
		a.docker.rmForce(ctx2, name)
		cancel2()

		resources := resourceLimitsFromMeta(meta)

		ctx3, cancel3 := context.WithTimeout(context.Background(), 60*time.Second)
		if err := a.startMinecraftContainer(ctx3, name, baseDir, hostPort, resources, nil); err != nil {
			cancel3()
			jsonWrite(w, 500, map[string]any{"ok": false, "error": "restart failed", "detail": err.Error()})
			return
		}
		cancel3()

		jsonWrite(w, 200, map[string]any{
			"ok":       true,
			"msg":      "applied",
			"server":   name,
			"hostPort": hostPort,
			"path":     target,
		})
		return
	}

	ctxD, cancelD := context.WithTimeout(context.Background(), 30*time.Minute)
	if err := a.dl.downloadToFile(ctxD, req.URL, target); err != nil {
		cancelD()
		jsonWrite(w, 500, map[string]any{"ok": false, "error": "download failed", "detail": err.Error()})
		return
	}
	cancelD()

	jsonWrite(w, 200, map[string]any{
		"ok":       true,
		"msg":      "downloaded",
		"server":   name,
		"path":     target,
		"destPath": destRel,
	})
}

type runtimeReq struct {
	Runtime  map[string]any `json:"runtime"`
	Template string         `json:"template"`
	Start    string         `json:"start"`
	Port     any            `json:"port"`
	NodeID   string         `json:"nodeId"`
}

func (a *Agent) handleRuntime(w http.ResponseWriter, r *http.Request, name string) {
	var req runtimeReq
	if err := readJSON(w, r, a.uploadLimit, &req); err != nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "bad-json", "detail": err.Error()})
		return
	}
	if name == "" {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "missing-name"})
		return
	}
	if req.Runtime == nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "missing-runtime"})
		return
	}
	if rawStartup, exists := req.Runtime["startupCommand"]; exists && strings.TrimSpace(fmt.Sprint(rawStartup)) != "" {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "raw docker startup commands are no longer supported"})
		return
	}
	if req.NodeID != "" && a.uuid != "" && req.NodeID != a.uuid {
		jsonWrite(w, 401, map[string]any{"ok": false, "error": "unauthorized"})
		return
	}

	serverDir := a.serverDir(name)

	existingMeta := a.loadMeta(serverDir)
	stripLegacyStartupCommandFields(existingMeta)

	hasStructuredRuntime := false
	if existingMeta != nil {
		if rt, ok := existingMeta["runtime"].(map[string]any); ok && len(rt) > 0 {
			hasStructuredRuntime = true
		}
	}

	if !hasStructuredRuntime {
		_ = os.RemoveAll(serverDir)
	}
	_ = ensureDir(serverDir)

	kind := strings.ToLower(strings.TrimSpace(req.Template))
	if kind == "" {
		kind = strings.ToLower(fmt.Sprint(req.Runtime["providerId"]))
	}

	finalStart := strings.TrimSpace(req.Start)
	if finalStart == "" {
		if kind == "python" {
			finalStart = "main.py"
		} else if kind == "nodejs" || kind == "discord-bot" {
			finalStart = "index.js"
		}
	}
	if finalStart != "" {
		finalStart = filepath.Base(finalStart)
		if finalStart == "." || finalStart == ".." {
			finalStart = ""
		}
	}

	hostPort := 0
	if req.Port != nil {
		parsedPort, ok := parseStrictPortValue(req.Port)
		if !ok {
			jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid-port"})
			return
		}
		hostPort = parsedPort
	}

	cleanRuntime := make(map[string]any)
	for _, key := range []string{"providerId", "versionId", "image", "tag", "restart"} {
		if raw, ok := req.Runtime[key]; ok {
			clean := strings.TrimSpace(fmt.Sprint(raw))
			if clean != "" && clean != "<nil>" {
				cleanRuntime[key] = clean
			}
		}
	}
	if rawCommand, ok := req.Runtime["command"]; ok {
		if clean := normalizeLegacyRuntimeProcessCommand(kind, fmt.Sprint(rawCommand), finalStart, defaultDataDirForType(kind)); clean != "" {
			if reason := validateRuntimeProcessCommand(clean); reason != "" {
				jsonWrite(w, 400, map[string]any{"ok": false, "error": reason})
				return
			}
			cleanRuntime["command"] = clean
		}
	}
	if rawEnv, ok := req.Runtime["env"].(map[string]any); ok && len(rawEnv) > 0 {
		cleanEnv := make(map[string]any)
		for key, value := range rawEnv {
			envKey := strings.TrimSpace(key)
			if envKey == "" {
				continue
			}
			envValue := strings.TrimSpace(fmt.Sprint(value))
			if envValue == "<nil>" {
				envValue = ""
			}
			cleanEnv[envKey] = envValue
		}
		if len(cleanEnv) > 0 {
			cleanRuntime["env"] = cleanEnv
		}
	}
	if rawVolumes, ok := req.Runtime["volumes"].([]any); ok && len(rawVolumes) > 0 {
		cleanVolumes := make([]any, 0, len(rawVolumes))
		for _, entry := range rawVolumes {
			spec := strings.TrimSpace(fmt.Sprint(entry))
			if spec == "" || spec == "<nil>" {
				continue
			}
			if strings.ContainsAny(spec, "\x00\r\n") {
				jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid-volume-spec"})
				return
			}
			cleanVolumes = append(cleanVolumes, spec)
		}
		if len(cleanVolumes) > 0 {
			cleanRuntime["volumes"] = cleanVolumes
		}
	}
	if rawPorts, ok := req.Runtime["ports"].([]any); ok && len(rawPorts) > 0 {
		cleanPorts := make([]any, 0, len(rawPorts))
		seenPorts := make(map[int]struct{}, len(rawPorts))
		for _, entry := range rawPorts {
			port, ok := parseStrictPortValue(entry)
			if !ok {
				jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid-runtime-port"})
				return
			}
			if _, seen := seenPorts[port]; seen {
				continue
			}
			seenPorts[port] = struct{}{}
			cleanPorts = append(cleanPorts, port)
		}
		if len(cleanPorts) > 0 {
			cleanRuntime["ports"] = cleanPorts
		}
	}

	switch kind {
	case "python":
		scaffoldPython(serverDir, finalStart)
	case "nodejs", "discord-bot":
		p := hostPort
		if p == 0 {
			p = 3001
		}
		scaffoldNode(serverDir, finalStart, p)
	default:
		if finalStart != "" {
			_ = os.WriteFile(filepath.Join(serverDir, finalStart), []byte(""), 0o644)
		}
	}

	meta := map[string]any{
		"type": func() string {
			if kind != "" {
				return kind
			}
			return "runtime"
		}(),
		"runtime": cleanRuntime,
		"start": func() any {
			if finalStart != "" {
				return finalStart
			}
			return nil
		}(),
		"hostPort": func() any {
			if hostPort > 0 {
				return hostPort
			}
			return nil
		}(),
		"updatedAt": time.Now().UnixMilli(),
	}

	if existingMeta != nil {
		if ca, ok := existingMeta["createdAt"]; ok {
			meta["createdAt"] = ca
		}
		if tpl, ok := existingMeta["template"]; ok {
			meta["template"] = tpl
		}
		if pf, ok := existingMeta["portForwards"]; ok {
			meta["portForwards"] = pf
		}
		if res, ok := existingMeta["resources"]; ok {
			meta["resources"] = res
		}
		if dig, ok := existingMeta["lastImageDigest"]; ok {
			meta["lastImageDigest"] = dig
		}
		if existingRt, ok := existingMeta["runtime"].(map[string]any); ok {
			newRt, _ := meta["runtime"].(map[string]any)
			if newRt == nil {
				newRt = make(map[string]any)
			}
			if img, ok := existingRt["image"]; ok && newRt["image"] == nil {
				newRt["image"] = img
			}
			if tag, ok := existingRt["tag"]; ok && newRt["tag"] == nil {
				newRt["tag"] = tag
			}
			meta["runtime"] = newRt
		}
	}

	_ = a.saveMeta(serverDir, meta)

	img := strings.TrimSpace(fmt.Sprint(req.Runtime["image"]))
	tag := strings.TrimSpace(fmt.Sprint(req.Runtime["tag"]))
	if img == "" {
		if kind == "python" {
			img = "python"
			if tag == "" {
				tag = "3.12-slim"
			}
		} else {
			img = "node"
			if tag == "" {
				tag = "20-alpine"
			}
		}
	}
	ref := img
	if tag != "" {
		ref = img + ":" + tag
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	_ = a.docker.pullImageAPI(ctx, ref)
	cancel()

	jsonWrite(w, 200, map[string]any{"ok": true, "type": meta["type"], "start": meta["start"], "hostPort": meta["hostPort"]})
}

func (a *Agent) handleStart(w http.ResponseWriter, r *http.Request, name string) {
	if !isValidContainerName(name) {
		jsonWrite(w, 400, map[string]any{"error": "invalid container name"})
		return
	}

	opMu := a.getServerOpLock(name)
	opMu.Lock()
	defer opMu.Unlock()

	serverDir := a.serverDir(name)
	if st, err := os.Stat(serverDir); err != nil || !st.IsDir() {
		jsonWrite(w, 404, map[string]any{"error": "not found"})
		return
	}
	meta := a.loadMeta(serverDir)
	if stripLegacyStartupCommandFields(meta) {
		_ = a.saveMeta(serverDir, meta)
	}
	if normalizeRustRuntimeForPterodactyl(meta) {
		_ = a.saveMeta(serverDir, meta)
	}
	typ := strings.ToLower(fmt.Sprint(meta["type"]))
	if a.debug {
		fmt.Printf("[handleStart] Server: %s, type: %s\n", name, typ)
	}

	var body struct {
		HostPort int               `json:"hostPort"`
		Env      map[string]string `json:"env"`
	}
	if err := readJSON(w, r, a.uploadLimit, &body); err != nil && !errors.Is(err, io.EOF) {
		jsonWrite(w, 400, map[string]any{"error": "invalid request body"})
		return
	}
	if body.HostPort != 0 {
		if _, ok := parseStrictPortValue(body.HostPort); !ok {
			jsonWrite(w, 400, map[string]any{"error": "invalid hostPort"})
			return
		}
	}

	resolveStartPort := func(defaultPort int) int {
		if body.HostPort > 0 {
			return body.HostPort
		}
		if storedPort, ok := parseStrictPortValue(meta["hostPort"]); ok {
			return storedPort
		}
		return defaultPort
	}

	resources := resourceLimitsFromMeta(meta)
	if a.logs != nil {
		a.logs.resetRecent(name)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	if a.stdinMgr != nil {
		a.stdinMgr.remove(name)
	}

	if typ == "minecraft" {
		hp := resolveStartPort(25565)
		enforceServerProps(serverDir)
		if err := a.startMinecraftContainer(ctx, name, serverDir, hp, resources, body.Env); err != nil {
			jsonWrite(w, 500, map[string]any{"error": "failed to start container", "detail": err.Error()})
			return
		}
		a.ensureLiveLogs(name)
		auditLog.Log("server_start", getClientIP(r), "", name, "start", "success", map[string]any{"type": "minecraft"})
		a.broadcastContainerEvent(name, "running")
		jsonWrite(w, 200, map[string]any{"ok": true})
		return
	}

	if meta["runtime"] != nil && (typ == "python" || typ == "nodejs" || typ == "discord-bot" || typ == "runtime") {
		hp := resolveStartPort(0)
		if err := a.startRuntimeContainer(ctx, name, serverDir, meta, hp, resources, body.Env); err != nil {
			jsonWrite(w, 500, map[string]any{"error": "failed to start container", "detail": err.Error()})
			return
		}
		a.ensureLiveLogs(name)
		auditLog.Log("server_start", getClientIP(r), "", name, "start", "success", map[string]any{"type": typ})
		a.broadcastContainerEvent(name, "running")
		jsonWrite(w, 200, map[string]any{"ok": true})
		return
	}

	if typ != "minecraft" && typ != "python" && typ != "nodejs" && typ != "discord-bot" && typ != "runtime" {
		hp := resolveStartPort(8080)

		if meta["runtime"] == nil {
			meta["runtime"] = make(map[string]any)
		}

		if err := a.startCustomDockerContainer(ctx, name, serverDir, meta, hp, resources); err != nil {
			jsonWrite(w, 500, map[string]any{"error": "failed to start container", "detail": err.Error()})
			return
		}
		a.ensureLiveLogs(name)
		auditLog.Log("server_start", getClientIP(r), "", name, "start", "success", map[string]any{"type": typ})
		a.broadcastContainerEvent(name, "running")
		jsonWrite(w, 200, map[string]any{"ok": true})
		return
	}

	jsonWrite(w, 400, map[string]any{"error": "unsupported template for start"})
}

func (a *Agent) handleStop(w http.ResponseWriter, r *http.Request, name string) {
	if !isValidContainerName(name) {
		jsonWrite(w, 400, map[string]any{"error": "invalid container name"})
		return
	}

	waitMode := r.URL.Query().Get("wait") == "true"

	opMu := a.getServerOpLock(name)
	opMu.Lock()

	a.stopMu.Lock()
	if !waitMode {
		if lastStop, ok := a.stoppingNow[name]; ok && time.Since(lastStop) < 5*time.Second {
			a.stopMu.Unlock()
			opMu.Unlock()
			jsonWrite(w, 200, map[string]any{"ok": true, "note": "already stopping"})
			return
		}
	}
	a.stoppingNow[name] = time.Now()
	a.stopMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !a.docker.containerExists(ctx, name) {
		opMu.Unlock()
		jsonWrite(w, 200, map[string]any{"ok": true, "note": "not running"})
		return
	}

	a.docker.updateRestartNo(ctx, name)

	if a.stdinMgr != nil {
		a.stdinMgr.remove(name)
	}

	serverDir := a.serverDir(name)
	meta := a.loadMeta(serverDir)
	serverType := strings.ToLower(fmt.Sprint(meta["type"]))
	isMinecraft := serverType == "" || serverType == "minecraft"

	if isMinecraft {
		_ = a.docker.sendCommand(ctx, a.stdinMgr, name, "stop", nil)
	}

	if waitMode {
		time.Sleep(a.stopGrace)
		ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel2()
		a.docker.stop(ctx2, name)
		if a.stdinMgr != nil {
			a.stdinMgr.remove(name)
		}
		a.stopMu.Lock()
		delete(a.stoppingNow, name)
		a.stopMu.Unlock()
		opMu.Unlock()
		a.broadcastContainerEvent(name, "stopped")
		jsonWrite(w, 200, map[string]any{"ok": true, "stopped": true})
		auditLog.Log("server_stop", getClientIP(r), "", name, "stop", "success", nil)
		return
	}

	stopRequestedAt := time.Now()
	opMu.Unlock()

	go func() {
		time.Sleep(a.stopGrace)

		opMu := a.getServerOpLock(name)
		opMu.Lock()
		defer opMu.Unlock()

		ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel2()
		startedAt, _, _, _ := a.docker.runCollect(ctx2, "inspect", "-f", "{{.State.StartedAt}}", name)
		if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(startedAt)); err == nil {
			if t.After(stopRequestedAt) {
				a.stopMu.Lock()
				delete(a.stoppingNow, name)
				a.stopMu.Unlock()
				return
			}
		}

		a.docker.stop(ctx2, name)
		if a.stdinMgr != nil {
			a.stdinMgr.remove(name)
		}
		a.stopMu.Lock()
		delete(a.stoppingNow, name)
		a.stopMu.Unlock()
	}()

	a.broadcastContainerEvent(name, "stopping")
	jsonWrite(w, 200, map[string]any{"ok": true, "stopping": true})
	auditLog.Log("server_stop", getClientIP(r), "", name, "stop", "success", nil)
}

func shouldRecreateContainerOnRestart(meta map[string]any) bool {
	if meta == nil {
		return false
	}
	template := strings.ToLower(strings.TrimSpace(fmt.Sprint(meta["type"])))
	if template == "" || template == "<nil>" {
		template = strings.ToLower(strings.TrimSpace(fmt.Sprint(meta["template"])))
	}
	if template == "rust" {
		return true
	}
	runtimeObj, _ := meta["runtime"].(map[string]any)
	if runtimeObj == nil {
		return false
	}
	image := strings.ToLower(strings.TrimSpace(fmt.Sprint(runtimeObj["image"])))
	tag := strings.ToLower(strings.TrimSpace(fmt.Sprint(runtimeObj["tag"])))
	imageRef := image + ":" + tag
	return strings.Contains(imageRef, "pterodactyl/games:rust") ||
		strings.Contains(imageRef, "parkervcp/games:rust") ||
		strings.Contains(imageRef, "didstopia/rust-server")
}

func (a *Agent) handleRestart(w http.ResponseWriter, r *http.Request, name string) {
	if !isValidContainerName(name) {
		jsonWrite(w, 400, map[string]any{"error": "invalid container name"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !a.docker.containerExists(ctx, name) {
		jsonWrite(w, 400, map[string]any{"error": "not running"})
		return
	}

	serverDir := a.serverDir(name)
	meta := a.loadMeta(serverDir)
	if normalizeRustRuntimeForPterodactyl(meta) {
		_ = a.saveMeta(serverDir, meta)
	}
	consoleMode := inferredConsoleModeForServer(meta)
	recreateForConsoleStdin := consoleMode == "stdin" && (!a.docker.containerOpenStdin(ctx, name) || !a.docker.containerTTY(ctx, name))
	if shouldRecreateContainerOnRestart(meta) || recreateForConsoleStdin {
		if a.stdinMgr != nil {
			a.stdinMgr.remove(name)
		}
		if a.logs != nil {
			a.logs.resetRecent(name)
		}
		startCtx, startCancel := context.WithTimeout(context.Background(), 240*time.Second)
		defer startCancel()
		resources := resourceLimitsFromMeta(meta)
		resolveStoredPort := func(defaultPort int) int {
			if storedPort, ok := parseStrictPortValue(meta["hostPort"]); ok {
				return storedPort
			}
			if runtimeObj, ok := meta["runtime"].(map[string]any); ok {
				if ports := runtimeIntSlice(runtimeObj["ports"]); len(ports) > 0 && ports[0] > 0 {
					return ports[0]
				}
			}
			return defaultPort
		}
		typ := strings.ToLower(strings.TrimSpace(fmt.Sprint(meta["type"])))
		if typ == "" || typ == "<nil>" {
			typ = strings.ToLower(strings.TrimSpace(fmt.Sprint(meta["template"])))
		}
		var startErr error
		switch {
		case typ == "minecraft":
			hostPort := resolveStoredPort(25565)
			enforceServerProps(serverDir)
			startErr = a.startMinecraftContainer(startCtx, name, serverDir, hostPort, resources, nil)
		case meta["runtime"] != nil && (typ == "python" || typ == "nodejs" || typ == "discord-bot" || typ == "runtime"):
			hostPort := resolveStoredPort(0)
			startErr = a.startRuntimeContainer(startCtx, name, serverDir, meta, hostPort, resources, nil)
		default:
			hostPort := resolveStoredPort(8080)
			startErr = a.startCustomDockerContainer(startCtx, name, serverDir, meta, hostPort, resources)
		}
		if startErr != nil {
			jsonWrite(w, 500, map[string]any{"error": "container recreate failed", "detail": startErr.Error()})
			return
		}
		a.ensureLiveLogs(name)
		a.broadcastContainerEvent(name, "running")
		jsonWrite(w, 200, map[string]any{"ok": true, "recreated": true, "reason": func() string {
			if recreateForConsoleStdin {
				return "stdin"
			}
			return "runtime"
		}()})
		return
	}

	if a.stdinMgr != nil {
		a.stdinMgr.remove(name)
	}
	if a.logs != nil {
		a.logs.resetRecent(name)
	}
	_, errStr, _, err := a.docker.runCollect(ctx, "restart", name)
	if err != nil {
		if a.debug {
			fmt.Printf("[docker] restart error for %s: %s\n", name, strings.TrimSpace(errStr))
		}
		jsonWrite(w, 500, map[string]any{"error": "container restart failed"})
		return
	}
	a.ensureLiveLogs(name)
	a.broadcastContainerEvent(name, "running")
	jsonWrite(w, 200, map[string]any{"ok": true})
}

func (a *Agent) broadcastConsoleChunk(name, prefix, chunk string) bool {
	cleaned := cleanLogLine(chunk)
	if cleaned == "" {
		return false
	}
	cleaned = strings.ReplaceAll(cleaned, "\r\n", "\n")
	cleaned = strings.ReplaceAll(cleaned, "\r", "\n")
	t := a.logs.getOrCreate(name)
	sent := false
	for _, line := range strings.Split(cleaned, "\n") {
		line = strings.TrimRight(line, " \t")
		plain := strings.TrimSpace(ansiSGRRE.ReplaceAllString(line, ""))
		if plain == "" {
			continue
		}
		a.logs.sendToClients(t, prefix+line)
		sent = true
	}
	return sent
}

func isMissingExecShell(stderr string, execPath string, exitCode int) bool {
	if exitCode != 126 && exitCode != 127 {
		return false
	}
	lower := strings.ToLower(stderr)
	base := strings.ToLower(filepath.Base(strings.TrimSpace(execPath)))
	if base == "" {
		return false
	}
	return (strings.Contains(lower, "executable file not found") ||
		strings.Contains(lower, "no such file or directory") ||
		strings.Contains(lower, "not found")) && strings.Contains(lower, base)
}

func (a *Agent) executeShellConsoleCommand(ctx context.Context, name, command string) (int, error) {
	a.broadcastConsoleChunk(name, "stdout:", "$ "+command+"\n")
	shells := []string{"sh", "/bin/sh", "bash", "/bin/bash"}
	for idx, shellPath := range shells {
		stdout, stderr, exitCode, err := a.docker.runCollect(ctx, "exec", name, shellPath, "-lc", command)
		sawOutput := false
		if stdout != "" {
			sawOutput = a.broadcastConsoleChunk(name, "stdout:", stdout) || sawOutput
		}
		if stderr != "" {
			sawOutput = a.broadcastConsoleChunk(name, "stderr:", stderr) || sawOutput
		}
		if err != nil && isMissingExecShell(stderr, shellPath, exitCode) && idx < len(shells)-1 {
			continue
		}
		if ctx.Err() != nil {
			a.broadcastConsoleChunk(name, "stderr:", fmt.Sprintf("[command timed out] %s\n", command))
			return exitCode, ctx.Err()
		}
		if err != nil {
			if !sawOutput {
				a.broadcastConsoleChunk(name, "stderr:", fmt.Sprintf("[command exited with code %d]\n", exitCode))
			}
			return exitCode, nil
		}
		if !sawOutput {
			a.broadcastConsoleChunk(name, "stdout:", "[command exited with code 0]\n")
		}
		return 0, nil
	}
	a.broadcastConsoleChunk(name, "stderr:", "[command failed] no supported shell found in container\n")
	return -1, fmt.Errorf("no supported shell found in container")
}

func sanitizeCommandRequestID(raw string) string {
	id := strings.TrimSpace(raw)
	if id == "" {
		return ""
	}
	if len(id) > consoleCommandRequestIDMaxLen {
		id = id[:consoleCommandRequestIDMaxLen]
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '-' || r == '_' || r == ':' || r == '.' {
			continue
		}
		return ""
	}
	return id
}

func (a *Agent) claimConsoleCommand(name, command, requestID string) (bool, string) {
	now := time.Now()

	a.commandMu.Lock()
	defer a.commandMu.Unlock()

	if a.recentCommands == nil {
		a.recentCommands = make(map[string]recentConsoleCommand)
	}

	for key, entry := range a.recentCommands {
		if now.Sub(entry.at) > consoleCommandRequestIDWindow {
			delete(a.recentCommands, key)
		}
	}
	if len(a.recentCommands) >= consoleCommandRecentMaxServers {
		var oldestKey string
		var oldestAt time.Time
		for key, entry := range a.recentCommands {
			if oldestKey == "" || entry.at.Before(oldestAt) {
				oldestKey = key
				oldestAt = entry.at
			}
		}
		if oldestKey != "" {
			delete(a.recentCommands, oldestKey)
		}
	}

	if entry, ok := a.recentCommands[name]; ok {
		if requestID != "" && entry.requestID == requestID && now.Sub(entry.at) <= consoleCommandRequestIDWindow {
			return false, "request_id"
		}
		if entry.command == command && now.Sub(entry.at) <= consoleCommandDuplicateWindow {
			return false, "rapid_duplicate"
		}
	}

	a.recentCommands[name] = recentConsoleCommand{
		command:   command,
		requestID: requestID,
		at:        now,
	}
	return true, ""
}

func (a *Agent) handleCommand(w http.ResponseWriter, r *http.Request, name string) {
	if !isValidContainerName(name) {
		jsonWrite(w, 400, map[string]any{"error": "invalid container name"})
		return
	}

	var body struct {
		Command   string `json:"command"`
		RequestID string `json:"requestId"`
	}
	if err := readJSON(w, r, 16*1024, &body); err != nil {
		jsonWrite(w, 400, map[string]any{"error": "bad json"})
		return
	}
	cmd := strings.TrimSpace(body.Command)
	if cmd == "" {
		jsonWrite(w, 400, map[string]any{"error": "missing command"})
		return
	}

	if len(cmd) > stdinCommandMaxLen {
		jsonWrite(w, 400, map[string]any{"error": "command too long", "max": stdinCommandMaxLen})
		return
	}
	requestID := sanitizeCommandRequestID(body.RequestID)

	serverDir := a.serverDir(name)
	if st, err := os.Stat(serverDir); err != nil || !st.IsDir() {
		jsonWrite(w, 404, map[string]any{"error": "not found"})
		return
	}

	if ok, reason := a.claimConsoleCommand(name, cmd, requestID); !ok {
		jsonWrite(w, 200, map[string]any{"ok": true, "deduplicated": true, "reason": reason})
		return
	}

	meta := a.loadMeta(serverDir)
	serverType := strings.ToLower(fmt.Sprint(meta["type"]))
	consoleMode := inferredConsoleModeForServer(meta)
	useExecShell := consoleMode == "exec" || consoleMode == "exec-shell" || consoleMode == "shell"

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if useExecShell {
		exitCode, err := a.executeShellConsoleCommand(ctx, name, cmd)
		if err != nil {
			jsonWrite(w, 200, map[string]any{
				"ok":       false,
				"error":    "failed-to-send",
				"detail":   err.Error(),
				"type":     serverType,
				"mode":     "exec",
				"exitCode": exitCode,
			})
			return
		}
		jsonWrite(w, 200, map[string]any{"ok": true, "type": serverType, "mode": "exec", "exitCode": exitCode})
		return
	}

	targetProcesses := consoleProcessTargetsForServer(meta)
	var liveAttachErr error
	if a.logs != nil && !preferDockerCLIPTYStdinTransport() {
		if err := a.logs.writeCommand(name, cmd); err == nil {
			jsonWrite(w, 200, map[string]any{"ok": true, "type": serverType, "mode": "stdin", "transport": "live-attach", "targeted": len(targetProcesses) > 0})
			return
		} else {
			liveAttachErr = err
			if a.debug {
				fmt.Printf("[docker] live attach stdin failed for %s: %v\n", name, err)
			}
		}
	}
	if err := a.docker.sendCommand(ctx, a.stdinMgr, name, cmd, targetProcesses); err != nil {
		detail := err.Error()
		if liveAttachErr != nil {
			detail = fmt.Sprintf("live attach failed: %v; %s", liveAttachErr, detail)
		}
		jsonWrite(w, 200, map[string]any{
			"ok":     false,
			"error":  "failed-to-send",
			"detail": detail,
			"type":   serverType,
			"mode":   "stdin",
		})
		return
	}

	transport := "attached-stdin"
	if liveAttachErr != nil {
		transport = "stdin-fallback"
	}
	jsonWrite(w, 200, map[string]any{"ok": true, "type": serverType, "mode": "stdin", "transport": transport, "targeted": len(targetProcesses) > 0})
}

func (a *Agent) handleServerCommandBody(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name      string `json:"name"`
		Command   string `json:"command"`
		RequestID string `json:"requestId"`
	}
	if err := readJSON(w, r, 16*1024, &body); err != nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "bad json"})
		return
	}
	name := sanitizeName(body.Name)
	if name == "" || strings.TrimSpace(body.Command) == "" {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "missing params"})
		return
	}
	r2 := *r
	r2.URL = &url.URL{Path: "/v1/servers/" + name + "/command"}
	commandBody, _ := json.Marshal(map[string]string{
		"command":   body.Command,
		"requestId": body.RequestID,
	})
	r2.Body = io.NopCloser(bytes.NewReader(commandBody))
	r2.ContentLength = int64(len(commandBody))
	a.handleCommand(w, &r2, name)
}

func (a *Agent) handleLogsSSE(w http.ResponseWriter, r *http.Request, name string) {
	if !isValidContainerName(name) {
		jsonWrite(w, 400, map[string]any{"error": "invalid container name"})
		return
	}

	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonWrite(w, 500, map[string]any{"error": "streaming not supported"})
		return
	}

	tailN := 200
	const maxLogTail = 10000
	if q := r.URL.Query().Get("tail"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n >= 0 {
			tailN = n
			if tailN > maxLogTail {
				tailN = maxLogTail
			}
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")

	sendEvent := func(ev string, data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev, string(b))
		flusher.Flush()
	}
	sendLine := func(line string, ts int64) {
		cleaned := cleanLogLine(line)
		if cleaned == "" {
			return
		}
		payload := map[string]any{"line": cleaned}
		if ts > 0 {
			payload["ts"] = ts
		}
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", string(b))
		flusher.Flush()
	}

	sendEvent("hello", map[string]any{"server": name, "ts": time.Now().UnixMilli(), "tail": tailN})
	t := a.logs.getOrCreate(name)

	if tailN > 0 {
		snapshot := []logSnapshotEntry{}
		snapshotFromDockerLogs := false
		if !preferDockerCLIConsoleTransport() {
			recentSnapshot := t.snapshotRecent(tailN)
			snapshot = make([]logSnapshotEntry, 0, len(recentSnapshot))
			for _, line := range recentSnapshot {
				snapshot = append(snapshot, logSnapshotEntry{line: line})
			}
		}
		if len(snapshot) == 0 {
			snapshotFromDockerLogs = true
			snapshot = a.logs.fetchTailSnapshotSingleflight(name, tailN, func() []logSnapshotEntry {
				ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
				out, errOut, _, _ := a.docker.runCollect(ctx, "logs", "--timestamps", "--tail", fmt.Sprint(tailN), name)
				cancel()
				entries := append(parseDockerLogSnapshotEntries(out, tailN), parseDockerLogSnapshotEntries(errOut, tailN)...)
				sort.SliceStable(entries, func(i, j int) bool {
					left := entries[i].ts
					right := entries[j].ts
					if left == 0 || right == 0 {
						return false
					}
					return left < right
				})
				if len(entries) > tailN {
					entries = entries[len(entries)-tailN:]
				}
				return entries
			})
		}
		if snapshotFromDockerLogs && len(snapshot) > 0 {
			lines := make([]string, 0, len(snapshot))
			for _, entry := range snapshot {
				lines = append(lines, entry.line)
			}
			t.appendRecentBatch(lines)
			t.markDockerSnapshotPrimed()
		}
		for _, entry := range snapshot {
			sendLine(entry.line, entry.ts)
		}
	}
	ch := make(chan string, 256)

	t.mu.Lock()
	t.clients[ch] = struct{}{}
	t.mu.Unlock()

	hb := time.NewTicker(15 * time.Second)
	defer hb.Stop()

	notify := r.Context().Done()
	for {
		select {
		case <-notify:
			t.mu.Lock()
			delete(t.clients, ch)
			t.mu.Unlock()
			a.logs.removeIfEmpty(t)
			return
		case line := <-ch:
			line = normalizeStreamedLogLine(line)
			sendLine(line, time.Now().UnixMilli())
		case <-hb.C:
			sendEvent("keepalive", map[string]any{"ts": time.Now().UnixMilli()})
		}
	}
}

func (a *Agent) handleKill(w http.ResponseWriter, r *http.Request, name string) {
	a.logs.killTail(name)
	a.logs.resetRecent(name)
	if a.stdinMgr != nil {
		a.stdinMgr.remove(name)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.docker.updateRestartNo(ctx, name)
	a.docker.rmForce(ctx, name)
	a.logs.resetRecent(name)
	a.broadcastContainerEvent(name, "stopped")
	jsonWrite(w, 200, map[string]any{"ok": true})
}

func (a *Agent) handleDeleteServer(w http.ResponseWriter, r *http.Request, name string) {
	if name == "" {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid name"})
		return
	}
	filesOnly := r.URL.Query().Get("files_only") == "true"
	if r.URL.Query().Get("async") == "true" {
		serverDir := a.serverDir(name)
		if filepath.Clean(serverDir) == filepath.Clean(a.volumesDir) {
			jsonWrite(w, 400, map[string]any{"ok": false, "error": "refusing to delete volumes root"})
			return
		}
		if !isUnder(a.volumesDir, serverDir) {
			jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid path"})
			return
		}
		clientIP := getClientIP(r)
		go func() {
			if _, err := a.performDeleteServer(name, filesOnly, clientIP); err != nil {
				fmt.Printf("[delete] async delete failed for %s: %v\n", name, err)
				auditLog.Log("server_delete", clientIP, "", name, "delete", "failure", map[string]any{"error": err.Error()})
			}
		}()
		jsonWrite(w, http.StatusAccepted, map[string]any{"ok": true, "name": name, "deleting": true})
		return
	}

	result, err := a.performDeleteServer(name, filesOnly, getClientIP(r))
	if err != nil {
		jsonWrite(w, 500, map[string]any{"ok": false, "error": "failed to queue server delete", "detail": err.Error()})
		return
	}
	jsonWrite(w, 200, map[string]any{"ok": true, "name": name, "deleting": result.ServerQueued || result.BackupQueued})
}

type deleteServerResult struct {
	ServerQueued bool
	BackupQueued bool
}

func (a *Agent) performDeleteServer(name string, filesOnly bool, clientIP string) (deleteServerResult, error) {
	var result deleteServerResult
	opMu := a.getServerOpLock(name)
	opMu.Lock()
	defer opMu.Unlock()

	a.logs.killTail(name)
	a.logs.resetRecent(name)
	if a.stdinMgr != nil {
		a.stdinMgr.remove(name)
	}

	if !filesOnly {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		a.docker.updateRestartNo(ctx, name)
		a.docker.rmForce(ctx, name)
		if ctx.Err() == nil {
			if err := a.docker.removeContainerAPI(ctx, dockerContainerName(name), true); err != nil {
				fmt.Printf("[delete] container removal warning for %s: %v\n", name, err)
			}
		}
		cancel()
		verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 5*time.Second)
		containerStillExists := a.docker.containerExists(verifyCtx, name)
		verifyCancel()
		if containerStillExists {
			fmt.Printf("[delete] container still exists for %s; queued retry cleanup\n", name)
			a.removeContainerEventually(name)
		}
		a.logs.resetRecent(name)
	}

	serverDir := a.serverDir(name)
	backupsDir := a.serverBackupsDir(name)
	fmt.Printf("[delete] attempting to detach server directory: %s\n", serverDir)

	if filepath.Clean(serverDir) == filepath.Clean(a.volumesDir) {
		return result, fmt.Errorf("refusing to delete volumes root")
	}
	if !isUnder(a.volumesDir, serverDir) {
		return result, fmt.Errorf("invalid path")
	}

	if _, err := os.Stat(serverDir); os.IsNotExist(err) {
		fmt.Printf("[delete] directory already gone: %s\n", serverDir)
		_ = os.Remove(a.metaPath(name))
		if isUnder(a.backupsDir(), backupsDir) {
			if detachedBackup, backupExists, detachErr := a.detachPathForAsyncDelete(a.backupsDir(), backupsDir, "backups", name); detachErr != nil {
				fmt.Printf("[delete] backup detach warning for %s: %v\n", name, detachErr)
			} else if backupExists {
				result.BackupQueued = true
				removeDetachedPathAsync("backups", detachedBackup)
			}
		}
		auditLog.Log("server_delete", clientIP, "", name, "delete", "success", nil)
		return result, nil
	}

	detachedServer, serverExists, err := a.detachPathForAsyncDelete(a.volumesDir, serverDir, "server", name)
	if err != nil {
		fmt.Printf("[delete] error detaching %s: %v\n", serverDir, err)
		return result, err
	}
	_ = os.Remove(a.metaPath(name))
	if serverExists {
		result.ServerQueued = true
		removeDetachedPathAsync("server", detachedServer)
	}

	if isUnder(a.backupsDir(), backupsDir) {
		if detachedBackup, backupExists, detachErr := a.detachPathForAsyncDelete(a.backupsDir(), backupsDir, "backups", name); detachErr != nil {
			fmt.Printf("[delete] backup detach warning for %s: %v\n", name, detachErr)
		} else if backupExists {
			result.BackupQueued = true
			removeDetachedPathAsync("backups", detachedBackup)
		}
	}

	fmt.Printf("[delete] queued async cleanup for %s (serverQueued=%v backupsQueued=%v)\n", name, result.ServerQueued, result.BackupQueued)
	auditLog.Log("server_delete", clientIP, "", name, "delete", "success", nil)
	return result, nil
}

func (a *Agent) removeContainerEventually(name string) {
	go func() {
		for attempt := 1; attempt <= 30; attempt++ {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			a.docker.updateRestartNo(ctx, name)
			a.docker.rmForce(ctx, name)
			err := a.docker.removeContainerAPI(ctx, dockerContainerName(name), true)
			cancel()

			checkCtx, checkCancel := context.WithTimeout(context.Background(), 5*time.Second)
			exists := a.docker.containerExists(checkCtx, name)
			checkCancel()
			if !exists {
				if err != nil {
					fmt.Printf("[delete] container retry cleanup completed for %s after transient error: %v\n", name, err)
				} else {
					fmt.Printf("[delete] container retry cleanup completed for %s\n", name)
				}
				return
			}

			if err != nil {
				fmt.Printf("[delete] container retry %d failed for %s: %v\n", attempt, name, err)
			} else {
				fmt.Printf("[delete] container retry %d did not remove %s yet\n", attempt, name)
			}
			delay := time.Duration(attempt) * time.Second
			if delay > time.Minute {
				delay = time.Minute
			}
			time.Sleep(delay)
		}
		fmt.Printf("[delete] container retry cleanup gave up for %s after repeated failures\n", name)
	}()
}

func (a *Agent) handleReinstallServer(w http.ResponseWriter, r *http.Request, name string) {
	if name == "" {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid name"})
		return
	}

	serverDir := a.serverDir(name)
	if filepath.Clean(serverDir) == filepath.Clean(a.volumesDir) {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "refusing to operate on volumes root"})
		return
	}
	if !isUnder(a.volumesDir, serverDir) {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid path"})
		return
	}

	var req struct {
		TemplateId     string                `json:"templateId"`
		Docker         *dockerTemplateConfig `json:"docker"`
		StartupCommand string                `json:"startupCommand"`
		HostPort       int                   `json:"hostPort"`
		JavaVersion    string                `json:"javaVersion"`
	}
	if err := readJSON(w, r, a.uploadLimit, &req); err != nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "bad json", "detail": err.Error()})
		return
	}
	if strings.TrimSpace(req.StartupCommand) != "" || (req.Docker != nil && strings.TrimSpace(req.Docker.StartupCommand) != "") {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "raw docker startup commands are no longer supported"})
		return
	}

	template := strings.ToLower(strings.TrimSpace(req.TemplateId))
	if template == "" {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "missing templateId"})
		return
	}
	if req.HostPort != 0 {
		if _, ok := parseStrictPortValue(req.HostPort); !ok {
			jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid hostPort"})
			return
		}
	}
	if req.Docker != nil {
		for _, p := range req.Docker.Ports {
			if _, ok := parseStrictPortValue(p); !ok {
				jsonWrite(w, 400, map[string]any{"ok": false, "error": fmt.Sprintf("invalid docker port: %d", p)})
				return
			}
		}
		if cleanPortProtocols := sanitizeDockerPortProtocols(req.Docker.PortProtocols, req.Docker.Ports); len(req.Docker.PortProtocols) > 0 && len(cleanPortProtocols) == 0 {
			jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid docker port protocols"})
			return
		}
	}

	if ok, why := a.verifyReinstallAdminRequest(r, name, template); !ok {
		auditLog.Log("server_reinstall", getClientIP(r), "", name, "reinstall", "denied", map[string]any{"reason": why})
		jsonWrite(w, 403, map[string]any{"ok": false, "error": why})
		return
	}

	fmt.Printf("[reinstall] Starting reinstall for server %s with template %s\n", name, template)

	a.logs.killTail(name)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	a.docker.updateRestartNo(ctx, name)
	a.docker.rmForce(ctx, name)
	cancel()
	fmt.Printf("[reinstall] Container removed for %s\n", name)

	oldMeta := a.loadMeta(serverDir)
	stripLegacyStartupCommandFields(oldMeta)
	existingResources, _ := oldMeta["resources"].(map[string]any)
	oldRuntime, _ := oldMeta["runtime"].(map[string]any)

	if _, err := os.Stat(serverDir); err == nil {
		if err := os.RemoveAll(serverDir); err != nil {
			fmt.Printf("[reinstall] Error removing server dir %s: %v\n", serverDir, err)
			jsonWrite(w, 500, map[string]any{"ok": false, "error": "failed to delete server files"})
			return
		}
		fmt.Printf("[reinstall] Server directory removed: %s\n", serverDir)
	}

	_ = os.Remove(a.metaPath(name))

	if err := ensureDir(serverDir); err != nil {
		jsonWrite(w, 500, map[string]any{"ok": false, "error": "failed to create server directory"})
		return
	}

	hostPort := req.HostPort
	if hostPort == 0 {
		if existingPort, ok := parseStrictPortValue(oldMeta["hostPort"]); ok {
			hostPort = existingPort
		} else if req.Docker != nil && len(req.Docker.Ports) > 0 {
			hostPort = req.Docker.Ports[0]
		} else if template == "minecraft" {
			hostPort = 25565
		} else {
			hostPort = 8080
		}
	}

	runtimeConfig := map[string]any{}
	rawJavaVersion := strings.TrimSpace(req.JavaVersion)
	if rawJavaVersion == "" && req.Docker != nil {
		rawJavaVersion = javaVersionFromDockerConfig(req.Docker)
	}
	if cleanJavaVersion := normalizeMinecraftJavaVersion(rawJavaVersion); strings.TrimSpace(rawJavaVersion) != "" {
		if cleanJavaVersion == "" {
			jsonWrite(w, 400, map[string]any{"ok": false, "error": "unsupported minecraft java version"})
			return
		}
		if template != "minecraft" {
			jsonWrite(w, 400, map[string]any{"ok": false, "error": "java version selection is only available for minecraft"})
			return
		}
		runtimeConfig["javaVersion"] = cleanJavaVersion
	}
	if req.Docker != nil {
		if req.Docker.Image != "" {
			runtimeConfig["image"] = req.Docker.Image
		}
		if req.Docker.Tag != "" {
			runtimeConfig["tag"] = req.Docker.Tag
		}
		if cmd := sanitizeRuntimeCommand(req.Docker.Command); cmd != "" {
			startFile := defaultStartFileForType(template)
			dataDir := defaultDataDirForType(template)
			cleanCmd := normalizeLegacyRuntimeProcessCommand(template, cmd, startFile, dataDir)
			if reason := validateRuntimeProcessCommand(cleanCmd); reason != "" {
				jsonWrite(w, 400, map[string]any{"ok": false, "error": reason})
				return
			}
			runtimeConfig["command"] = cleanCmd
		}
		if req.Docker.Ports != nil {
			runtimeConfig["ports"] = req.Docker.Ports
		}
		if portProtocols := sanitizeDockerPortProtocols(req.Docker.PortProtocols, req.Docker.Ports); len(portProtocols) > 0 {
			runtimeConfig["portProtocols"] = portProtocols
		}
		if req.Docker.Volumes != nil {
			runtimeConfig["volumes"] = req.Docker.Volumes
		}
		if req.Docker.Env != nil {
			runtimeConfig["env"] = req.Docker.Env
		}
		if startup := startupTemplateFromDockerConfig(req.Docker); startup != "" {
			runtimeConfig["adpanelStartup"] = startup
			delete(runtimeConfig, "command")
		}
		if startupDisplay := startupDisplayFromDockerConfig(req.Docker); startupDisplay != "" {
			runtimeConfig["startupDisplay"] = startupDisplay
		}
		if install := dockerTemplateInstall(req.Docker); install != nil {
			runtimeConfig["install"] = install
		}
		reqWorkdir := strings.TrimSpace(req.Docker.Workdir)
		if reqWorkdir == "" {
			reqWorkdir = strings.TrimSpace(req.Docker.WorkingDir)
		}
		if reqWorkdir != "" {
			cleanWorkdir := normalizeContainerWorkdir(reqWorkdir)
			if cleanWorkdir == "" {
				jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid workdir (must be an absolute path like /app)"})
				return
			}
			runtimeConfig["workdir"] = cleanWorkdir
		}
		if restart := strings.TrimSpace(req.Docker.Restart); restart != "" {
			runtimeConfig["restart"] = restart
		} else if restartPolicy := strings.TrimSpace(req.Docker.RestartPolicy); restartPolicy != "" {
			runtimeConfig["restart"] = restartPolicy
		}
		if req.Docker.RunAsRoot || req.Docker.RunAsRootSnake {
			runtimeConfig["runAsRoot"] = true
		}
		if req.Docker.Console != nil {
			runtimeConfig["console"] = req.Docker.Console
		}
		runtimeConfig["providerId"] = "custom"
	}
	if oldRuntime != nil {
		for _, key := range []string{"providerId", "versionId", "image", "tag", "javaVersion", "restart", "adpanelStartup", "install", "portProtocols", "runAsRoot"} {
			if _, exists := runtimeConfig[key]; exists {
				continue
			}
			if key == "portProtocols" {
				if protocols := runtimePortProtocolMap(oldRuntime[key]); len(protocols) > 0 {
					clean := map[string][]string{}
					for port, values := range protocols {
						clean[strconv.Itoa(port)] = values
					}
					runtimeConfig[key] = clean
				}
				continue
			}
			if key == "install" {
				if install := installConfigFromRuntime(oldRuntime); install != nil {
					runtimeConfig[key] = install
				}
				continue
			}
			if key == "runAsRoot" {
				if runtimeBool(oldRuntime[key]) {
					runtimeConfig[key] = true
				}
				continue
			}
			value := strings.TrimSpace(fmt.Sprint(oldRuntime[key]))
			if key == "adpanelStartup" && (value == "" || value == "<nil>") {
				value = startupTemplateFromRuntime(oldRuntime)
			}
			if value != "" && value != "<nil>" {
				runtimeConfig[key] = value
			}
		}
		if _, exists := runtimeConfig["command"]; !exists {
			if cmd := sanitizeRuntimeCommand(oldRuntime["command"]); cmd != "" {
				startFile := defaultStartFileForType(template)
				dataDir := defaultDataDirForType(template)
				cleanCmd := normalizeLegacyRuntimeProcessCommand(template, cmd, startFile, dataDir)
				if validateRuntimeProcessCommand(cleanCmd) == "" {
					runtimeConfig["command"] = cleanCmd
				}
			}
		}
		if _, exists := runtimeConfig["ports"]; !exists {
			if rawPorts, ok := oldRuntime["ports"].([]any); ok && len(rawPorts) > 0 {
				cleanPorts := make([]any, 0, len(rawPorts))
				seenPorts := make(map[int]struct{}, len(rawPorts))
				for _, entry := range rawPorts {
					port, ok := parseStrictPortValue(entry)
					if !ok {
						continue
					}
					if _, seen := seenPorts[port]; seen {
						continue
					}
					seenPorts[port] = struct{}{}
					cleanPorts = append(cleanPorts, port)
				}
				if len(cleanPorts) > 0 {
					runtimeConfig["ports"] = cleanPorts
				}
			}
		}
		if _, exists := runtimeConfig["volumes"]; !exists {
			if rawVolumes, ok := oldRuntime["volumes"].([]any); ok && len(rawVolumes) > 0 {
				cleanVolumes := make([]any, 0, len(rawVolumes))
				for _, entry := range rawVolumes {
					spec := strings.TrimSpace(fmt.Sprint(entry))
					if spec == "" || spec == "<nil>" || strings.ContainsAny(spec, "\x00\r\n") {
						continue
					}
					cleanVolumes = append(cleanVolumes, spec)
				}
				if len(cleanVolumes) > 0 {
					runtimeConfig["volumes"] = cleanVolumes
				}
			}
		}
		if _, exists := runtimeConfig["env"]; !exists {
			if rawEnv, ok := oldRuntime["env"].(map[string]any); ok && len(rawEnv) > 0 {
				cleanEnv := make(map[string]any)
				for key, value := range rawEnv {
					envKey := strings.TrimSpace(key)
					if envKey == "" {
						continue
					}
					envValue := strings.TrimSpace(fmt.Sprint(value))
					if envValue == "<nil>" {
						envValue = ""
					}
					cleanEnv[envKey] = envValue
				}
				if len(cleanEnv) > 0 {
					runtimeConfig["env"] = cleanEnv
				}
			}
		}
		if _, exists := runtimeConfig["workdir"]; !exists {
			oldWorkdir := strings.TrimSpace(fmt.Sprint(oldRuntime["workdir"]))
			if oldWorkdir == "" || oldWorkdir == "<nil>" {
				oldWorkdir = strings.TrimSpace(fmt.Sprint(oldRuntime["workingDir"]))
			}
			if cleanWorkdir := normalizeContainerWorkdir(oldWorkdir); cleanWorkdir != "" {
				runtimeConfig["workdir"] = cleanWorkdir
			}
		}
		if _, exists := runtimeConfig["console"]; !exists {
			if rawConsole, ok := oldRuntime["console"]; ok && rawConsole != nil {
				runtimeConfig["console"] = rawConsole
			}
		}
	}

	startFile := strings.TrimSpace(fmt.Sprint(oldMeta["start"]))
	if startFile == "" {
		startFile = defaultStartFileForType(template)
	}

	meta := map[string]any{
		"type":      template,
		"template":  template,
		"hostPort":  hostPort,
		"createdAt": time.Now().UnixMilli(),
	}
	if oldCreatedAt, ok := oldMeta["createdAt"]; ok {
		meta["createdAt"] = oldCreatedAt
	}
	if startFile != "" {
		meta["start"] = startFile
	}
	if existingResources != nil && len(existingResources) > 0 {
		meta["resources"] = existingResources
	}
	if existingPF, ok := oldMeta["portForwards"]; ok {
		meta["portForwards"] = existingPF
	}

	if template == "minecraft" {
		fork := "paper"
		ver := "1.21.8"
		writeMinecraftScaffold(serverDir, name, fork, ver)

		dlCtx, dlCancel := context.WithTimeout(context.Background(), 30*time.Second)
		jarURL := getMinecraftJarURL(dlCtx, a.dl, fork, ver, a.debug)
		dlCancel()

		dlCtx2, dlCancel2 := context.WithTimeout(context.Background(), 30*time.Minute)
		if err := a.dl.downloadToFile(dlCtx2, jarURL, filepath.Join(serverDir, "server.jar")); err != nil {
			dlCancel2()
			fmt.Printf("[reinstall] Failed to download minecraft jar: %v\n", err)
			jsonWrite(w, 500, map[string]any{"ok": false, "error": "download failed", "detail": err.Error()})
			return
		}
		dlCancel2()

		mcImage := defaultMinecraftProcessImage
		mcTag := defaultMinecraftProcessTag
		mcCommand := defaultMinecraftProcessCommand
		mcWorkdir := defaultDataDirForType("minecraft")
		mcJavaVersion := normalizeMinecraftJavaVersion(runtimeConfig["javaVersion"])
		if image, ok := runtimeConfig["image"].(string); ok && strings.TrimSpace(image) != "" {
			mcImage = strings.TrimSpace(image)
		}
		if tag, ok := runtimeConfig["tag"].(string); ok && strings.TrimSpace(tag) != "" {
			mcTag = strings.TrimSpace(tag)
		}
		if cmd, ok := runtimeConfig["command"].(string); ok && strings.TrimSpace(cmd) != "" {
			mcCommand = strings.TrimSpace(cmd)
		}
		if workdir, ok := runtimeConfig["workdir"].(string); ok && strings.TrimSpace(workdir) != "" {
			mcWorkdir = strings.TrimSpace(workdir)
		}
		if mcJavaVersion == "" {
			mcJavaVersion = minecraftJavaVersionFromTag(mcTag)
		}
		if mcJavaVersion != "" {
			mcImage = defaultMinecraftProcessImage
			mcTag = minecraftJavaTag(mcJavaVersion)
		}

		mcRuntime := map[string]any{
			"image":       mcImage,
			"tag":         mcTag,
			"command":     mcCommand,
			"workdir":     mcWorkdir,
			"javaVersion": mcJavaVersion,
		}
		for _, key := range []string{"providerId", "versionId", "ports", "volumes", "env", "restart", "console"} {
			if value, ok := runtimeConfig[key]; ok && value != nil {
				mcRuntime[key] = value
			}
		}

		meta = map[string]any{
			"type":     "minecraft",
			"fork":     fork,
			"version":  ver,
			"start":    "server.jar",
			"hostPort": hostPort,
			"runtime":  mcRuntime,
		}
		if oldCreatedAt, ok := oldMeta["createdAt"]; ok {
			meta["createdAt"] = oldCreatedAt
		}
		if existingResources != nil && len(existingResources) > 0 {
			meta["resources"] = existingResources
		}
		if existingPF, ok := oldMeta["portForwards"]; ok {
			meta["portForwards"] = existingPF
		}
		_ = a.saveMeta(serverDir, meta)
		enforceServerProps(serverDir)
	} else {
		if cmd, ok := runtimeConfig["command"].(string); !ok || strings.TrimSpace(cmd) == "" {
			if defaultCmd := defaultRuntimeCommandForType(template, startFile, defaultDataDirForType(template)); defaultCmd != "" {
				runtimeConfig["command"] = defaultCmd
			}
		}
		if _, ok := runtimeConfig["workdir"]; !ok && hasManagedRuntimeDefaults(template) {
			runtimeConfig["workdir"] = defaultDataDirForType(template)
		}
		if len(runtimeConfig) > 0 {
			meta["runtime"] = runtimeConfig
		}

		if install := installConfigFromRuntime(runtimeConfig); install != nil {
			if err := a.runTemplateInstall(name, serverDir, install, runtimeConfig, hostPort, nil); err != nil {
				fmt.Printf("[reinstall] Template install failed: %v\n", err)
				jsonWrite(w, 500, map[string]any{"ok": false, "error": "install failed", "detail": err.Error()})
				return
			}
		}

		switch template {
		case "python":
			scaffoldPython(serverDir, startFile)
		case "nodejs", "discord-bot":
			p := hostPort
			if p == 0 {
				p = 3001
			}
			scaffoldNode(serverDir, startFile, p)
		default:
			if startFile != "" {
				_ = os.WriteFile(filepath.Join(serverDir, filepath.Base(startFile)), []byte(""), 0o644)
			}
		}

		_ = a.saveMeta(serverDir, meta)

		if img, ok := runtimeConfig["image"].(string); ok && strings.TrimSpace(img) != "" {
			tag := "latest"
			if t, ok := runtimeConfig["tag"].(string); ok && strings.TrimSpace(t) != "" {
				tag = strings.TrimSpace(t)
			}
			imageRef := strings.TrimSpace(img) + ":" + tag
			pullCtx, pullCancel := context.WithTimeout(context.Background(), 10*time.Minute)
			if err := a.docker.ensureImageAvailableAPI(pullCtx, imageRef); err != nil {
				pullCancel()
				jsonWrite(w, 500, map[string]any{"ok": false, "error": "docker image pull failed", "detail": err.Error()})
				return
			}
			pullCancel()
			ensureVolumeWritable(serverDir)
		}
	}

	fmt.Printf("[reinstall] Server %s reinstalled successfully with template %s\n", name, template)
	auditLog.Log("server_reinstall", getClientIP(r), "", name, "reinstall", "success", map[string]any{"template": template})
	jsonWrite(w, 200, map[string]any{"ok": true, "name": name, "template": template, "meta": meta})
}

func (a *Agent) handleFilesList(w http.ResponseWriter, r *http.Request, name string) {
	root := a.serverDir(name)
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		jsonWrite(w, 404, map[string]any{"error": "server not found"})
		return
	}
	rel := r.URL.Query().Get("path")
	dir, ok := safeJoin(root, rel)
	if !ok {
		jsonWrite(w, 400, map[string]any{"error": "invalid path"})
		return
	}
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() {
		jsonWrite(w, 400, map[string]any{"error": "not a directory"})
		return
	}
	if err := ensureResolvedUnder(root, dir); err != nil {
		jsonWrite(w, 400, map[string]any{"error": "symlink escape blocked"})
		return
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		if a.debug {
			fmt.Printf("[files] list error for %s: %v\n", dir, err)
		}
		jsonWrite(w, 500, map[string]any{"error": "failed to read directory"})
		return
	}
	out := make([]map[string]any, 0, len(ents))
	for _, e := range ents {
		typ := "file"
		if e.IsDir() {
			typ = "dir"
		}
		out = append(out, map[string]any{"name": e.Name(), "type": typ})
	}
	jsonWrite(w, 200, map[string]any{"ok": true, "entries": out, "path": rel})
}

func (a *Agent) handleFilesRead(w http.ResponseWriter, r *http.Request, name string) {
	root := a.serverDir(name)
	rel := r.URL.Query().Get("path")
	file, ok := safeJoin(root, rel)
	if !ok {
		jsonWrite(w, 400, map[string]any{"error": "invalid path"})
		return
	}
	info, err := os.Lstat(file)
	if err != nil {
		jsonWrite(w, 404, map[string]any{"error": "file not found"})
		return
	}
	if info.Mode()&os.ModeSymlink != 0 {
		jsonWrite(w, 400, map[string]any{"error": "cannot read symlink"})
		return
	}
	if err := ensureResolvedUnder(root, file); err != nil {
		jsonWrite(w, 400, map[string]any{"error": "symlink escape blocked"})
		return
	}
	if info.IsDir() {
		jsonWrite(w, 400, map[string]any{"error": "path is a directory"})
		return
	}
	const maxEditorRead = 10 << 20
	if info.Size() > maxEditorRead {
		jsonWrite(w, 400, map[string]any{"error": "file too large (max 10MB)"})
		return
	}
	b, err := os.ReadFile(file)
	if err != nil {
		if a.debug {
			fmt.Printf("[files] read error for %s: %v\n", file, err)
		}
		jsonWrite(w, 500, map[string]any{"error": "failed to read file"})
		return
	}
	jsonWrite(w, 200, map[string]any{"ok": true, "path": rel, "content": string(b)})
}

func (a *Agent) handleFilesWrite(w http.ResponseWriter, r *http.Request, name string) {
	root := a.serverDir(name)
	var body struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := readJSON(w, r, 10<<20, &body); err != nil {
		jsonWrite(w, 400, map[string]any{"error": "bad json or content too large (max 10MB)"})
		return
	}
	if diskErr := a.checkDiskLimit(root, int64(len(body.Content))); diskErr != nil {
		jsonWrite(w, 507, diskErr)
		return
	}
	file, ok := safeJoin(root, body.Path)
	if !ok {
		jsonWrite(w, 400, map[string]any{"error": "invalid path"})
		return
	}

	parentDir := filepath.Dir(file)
	if parentDir != root {
		resolvedParent, err := filepath.EvalSymlinks(parentDir)
		if err == nil && !isUnder(root, resolvedParent) {
			jsonWrite(w, 400, map[string]any{"error": "symlink escape blocked"})
			return
		}
	}

	_ = ensureDir(filepath.Dir(file))
	if rp, err := filepath.EvalSymlinks(filepath.Dir(file)); err == nil && !isUnder(root, rp) {
		jsonWrite(w, 400, map[string]any{"error": "symlink escape blocked"})
		return
	}
	if info, err := os.Lstat(file); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			jsonWrite(w, 400, map[string]any{"error": "cannot write to symlink"})
			return
		}
	}
	if err := writeFileNoFollow(file, []byte(body.Content), 0o644); err != nil {
		jsonWrite(w, 500, map[string]any{"error": "failed to write file"})
		return
	}
	jsonWrite(w, 200, map[string]any{"ok": true})
}

func (a *Agent) handleFilesDelete(w http.ResponseWriter, r *http.Request, name string) {
	root := a.serverDir(name)
	var body struct {
		Path  string `json:"path"`
		IsDir bool   `json:"isDir"`
	}
	if err := readJSON(w, r, a.uploadLimit, &body); err != nil {
		jsonWrite(w, 400, map[string]any{"error": "bad json"})
		return
	}
	p, ok := safeJoin(root, body.Path)
	if !ok {
		jsonWrite(w, 400, map[string]any{"error": "invalid path"})
		return
	}
	if filepath.Clean(p) == filepath.Clean(root) {
		jsonWrite(w, 400, map[string]any{"error": "refusing to delete server root"})
		return
	}
	if _, err := os.Lstat(p); err != nil {
		jsonWrite(w, 200, map[string]any{"ok": true, "note": "already missing"})
		return
	}
	if err := ensureResolvedUnder(root, p); err != nil {
		jsonWrite(w, 400, map[string]any{"error": "symlink escape blocked"})
		return
	}
	parentDir := filepath.Dir(p)
	if parentDir != root {
		resolvedParent, rpErr := filepath.EvalSymlinks(parentDir)
		if rpErr == nil && !isUnder(root, resolvedParent) {
			jsonWrite(w, 400, map[string]any{"error": "symlink escape blocked"})
			return
		}
	}
	var err error
	if body.IsDir {
		err = os.RemoveAll(p)
	} else {
		err = os.Remove(p)
	}
	if err != nil {
		jsonWrite(w, 500, map[string]any{"error": "delete operation failed"})
		return
	}
	jsonWrite(w, 200, map[string]any{"ok": true})
}

func (a *Agent) handleFilesUpload(w http.ResponseWriter, r *http.Request, name string) {
	root := a.serverDir(name)
	var body struct {
		DestDir  string `json:"destDir"`
		Filename string `json:"filename"`
		Content  string `json:"content"`
	}
	if err := readJSON(w, r, a.uploadLimit, &body); err != nil {
		jsonWrite(w, 400, map[string]any{"error": "bad json"})
		return
	}
	if diskErr := a.checkDiskLimit(root, int64(len(body.Content))); diskErr != nil {
		jsonWrite(w, 507, diskErr)
		return
	}
	if strings.TrimSpace(body.Filename) == "" {
		jsonWrite(w, 400, map[string]any{"error": "missing filename"})
		return
	}
	destDir, ok := safeJoin(root, body.DestDir)
	if !ok {
		jsonWrite(w, 400, map[string]any{"error": "invalid path"})
		return
	}
	_ = ensureDir(destDir)
	if err := ensureResolvedUnder(root, destDir); err != nil {
		jsonWrite(w, 400, map[string]any{"error": "symlink escape blocked"})
		return
	}

	fn := filepath.Base(body.Filename)
	fn = strings.NewReplacer("\r", "_", "\n", "_", "\x00", "_").Replace(fn)
	if fn == "" || fn == "." || fn == ".." {
		jsonWrite(w, 400, map[string]any{"error": "invalid filename"})
		return
	}
	destFile := filepath.Join(destDir, fn)
	if !isUnder(destDir, destFile) {
		jsonWrite(w, 400, map[string]any{"error": "invalid filename"})
		return
	}

	b64 := body.Content
	if strings.HasPrefix(b64, "data:") {
		if i := strings.Index(b64, ","); i >= 0 {
			b64 = b64[i+1:]
		}
	}
	const maxBase64Upload = 25 << 20
	if base64.StdEncoding.DecodedLen(len(b64)) > maxBase64Upload {
		jsonWrite(w, 400, map[string]any{"error": "file too large for base64 upload; use stream upload"})
		return
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		jsonWrite(w, 400, map[string]any{"error": "invalid base64"})
		return
	}
	if err := writeFileNoFollow(destFile, data, 0o644); err != nil {
		if a.debug {
			fmt.Printf("[files] upload error for %s: %v\n", destFile, err)
		}
		jsonWrite(w, 500, map[string]any{"error": "failed to write file"})
		return
	}
	rel, _ := filepath.Rel(root, destFile)
	jsonWrite(w, 200, map[string]any{"ok": true, "path": rel})
}

func (a *Agent) handleFilesArchive(w http.ResponseWriter, r *http.Request, name string) {
	root := a.serverDir(name)
	var body struct {
		Paths   []string `json:"paths"`
		DestDir string   `json:"destDir"`
	}
	if err := readJSON(w, r, a.uploadLimit, &body); err != nil {
		jsonWrite(w, 400, map[string]any{"error": "bad json"})
		return
	}
	if len(body.Paths) == 0 {
		jsonWrite(w, 400, map[string]any{"error": "no paths specified"})
		return
	}
	if diskErr := a.checkDiskLimit(root, 0); diskErr != nil {
		jsonWrite(w, 507, diskErr)
		return
	}

	destDir, ok := safeJoin(root, body.DestDir)
	if !ok {
		jsonWrite(w, 400, map[string]any{"error": "invalid destination path"})
		return
	}
	_ = ensureDir(destDir)
	resolvedDest, rdErr := filepath.EvalSymlinks(destDir)
	if rdErr == nil && !isUnder(root, resolvedDest) {
		jsonWrite(w, 400, map[string]any{"error": "symlink escape blocked"})
		return
	}

	archiveName := fmt.Sprintf("archive-%s.tar.gz", time.Now().Format("20060102-150405"))
	archivePath := filepath.Join(destDir, archiveName)

	outFile, err := os.OpenFile(archivePath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		jsonWrite(w, 500, map[string]any{"error": "failed to create archive"})
		return
	}
	defer outFile.Close()

	gw := gzip.NewWriter(outFile)
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	for _, p := range body.Paths {
		srcPath, ok := safeJoin(root, p)
		if !ok {
			continue
		}
		if err := ensureResolvedUnder(root, srcPath); err != nil {
			continue
		}
		info, err := os.Lstat(srcPath)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		baseName := filepath.Base(srcPath)

		if info.IsDir() {
			_ = filepath.WalkDir(srcPath, func(path string, d os.DirEntry, walkErr error) error {
				if walkErr != nil || d == nil {
					return nil
				}
				if d.Type()&os.ModeSymlink != 0 {
					return nil
				}
				fi, fiErr := d.Info()
				if fiErr != nil {
					return nil
				}
				rel, relErr := filepath.Rel(srcPath, path)
				if relErr != nil {
					return nil
				}
				if rel == "." {
					return nil
				}
				if fi.Mode()&os.ModeSymlink != 0 {
					return nil
				}
				if resolved, rErr := filepath.EvalSymlinks(path); rErr == nil && !isUnder(root, resolved) {
					return nil
				}
				hdr, hdrErr := tar.FileInfoHeader(fi, "")
				if hdrErr != nil {
					return nil
				}
				hdr.Mode = int64(sanitizeFileMode(os.FileMode(hdr.Mode), fi.IsDir()))
				hdr.Uid = 0
				hdr.Gid = 0
				hdr.Uname = ""
				hdr.Gname = ""
				hdr.Name = filepath.ToSlash(filepath.Join(baseName, rel))
				if fi.IsDir() && !strings.HasSuffix(hdr.Name, "/") {
					hdr.Name += "/"
				}
				if err := tw.WriteHeader(hdr); err != nil {
					return nil
				}
				if !fi.Mode().IsRegular() {
					return nil
				}
				f, openErr := os.Open(path)
				if openErr != nil {
					return nil
				}
				_, _ = io.Copy(tw, f)
				f.Close()
				return nil
			})
		} else {
			if info.Mode()&os.ModeSymlink != 0 {
				continue
			}
			hdr, hdrErr := tar.FileInfoHeader(info, "")
			if hdrErr != nil {
				continue
			}
			hdr.Mode = int64(sanitizeFileMode(os.FileMode(hdr.Mode), false))
			hdr.Uid = 0
			hdr.Gid = 0
			hdr.Uname = ""
			hdr.Gname = ""
			hdr.Name = baseName
			if err := tw.WriteHeader(hdr); err != nil {
				continue
			}
			if info.Mode().IsRegular() {
				f, openErr := os.Open(srcPath)
				if openErr != nil {
					continue
				}
				_, _ = io.Copy(tw, f)
				f.Close()
			}
		}
	}

	if err := tw.Close(); err != nil {
		jsonWrite(w, 500, map[string]any{"error": "failed to finalize archive"})
		return
	}
	if err := gw.Close(); err != nil {
		jsonWrite(w, 500, map[string]any{"error": "failed to finalize archive"})
		return
	}

	relPath, _ := filepath.Rel(root, archivePath)
	jsonWrite(w, 200, map[string]any{"ok": true, "path": relPath, "name": archiveName})
}

func (a *Agent) backupsDir() string {
	return filepath.Join(a.dataRoot, "backups")
}

func (a *Agent) serverBackupsDir(serverName string) string {
	return filepath.Join(a.backupsDir(), sanitizeName(serverName))
}

func deletionTrashDir(parent string) string {
	return filepath.Join(parent, ".adpanel_deleting")
}

func deletionTombstoneName(label, name string) string {
	safe := sanitizeName(name)
	if safe == "" {
		safe = "unknown"
	}
	label = sanitizeName(label)
	if label == "" {
		label = "path"
	}
	return fmt.Sprintf("%s-%s-%d", label, safe, time.Now().UnixNano())
}

func deleteTrashPrefix(label, name string) string {
	safe := sanitizeName(name)
	if safe == "" {
		safe = "unknown"
	}
	label = sanitizeName(label)
	if label == "" {
		label = "path"
	}
	return fmt.Sprintf("%s-%s-", label, safe)
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func deleteTrashEntryExists(parentRoot, label, name string) bool {
	entries, err := os.ReadDir(deletionTrashDir(parentRoot))
	if err != nil {
		return false
	}
	prefix := deleteTrashPrefix(label, name)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			return true
		}
	}
	return false
}

func (a *Agent) containerExistsKnown(ctx context.Context, name string) (bool, bool) {
	_, _, _, err := a.docker.runCollect(ctx, "inspect", name)
	if err == nil {
		return true, true
	}
	if ctx.Err() != nil {
		return false, false
	}
	return false, true
}

func (a *Agent) deleteStatus(ctx context.Context, name string) map[string]any {
	containerExists, containerCheckOk := a.containerExistsKnown(ctx, name)
	serverDirExists := pathExists(a.serverDir(name))
	metaExists := pathExists(a.metaPath(name))
	backupsDir := a.serverBackupsDir(name)
	backupsDirExists := false
	if isUnder(a.backupsDir(), backupsDir) {
		backupsDirExists = pathExists(backupsDir)
	}
	serverTrashExists := deleteTrashEntryExists(a.volumesDir, "server", name)
	backupTrashExists := deleteTrashEntryExists(a.backupsDir(), "backups", name)
	deleted := containerCheckOk && !containerExists && !serverDirExists && !metaExists && !backupsDirExists
	return map[string]any{
		"ok":                true,
		"deleteStatus":      true,
		"name":              name,
		"deleted":           deleted,
		"containerExists":   containerExists,
		"containerCheckOk":  containerCheckOk,
		"serverDirExists":   serverDirExists,
		"metaExists":        metaExists,
		"backupsDirExists":  backupsDirExists,
		"serverTrashExists": serverTrashExists,
		"backupTrashExists": backupTrashExists,
	}
}

func (a *Agent) handleDeleteStatus(w http.ResponseWriter, r *http.Request, name string) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	jsonWrite(w, 200, a.deleteStatus(ctx, name))
}

func (a *Agent) detachPathForAsyncDelete(parentRoot, target, label, name string) (string, bool, error) {
	parentRoot = filepath.Clean(parentRoot)
	target = filepath.Clean(target)
	if parentRoot == "" || target == "" || target == "." || target == string(os.PathSeparator) {
		return "", false, fmt.Errorf("invalid delete path")
	}
	if filepath.Clean(parentRoot) == target {
		return "", false, fmt.Errorf("refusing to delete root")
	}
	if !isUnder(parentRoot, target) {
		return "", false, fmt.Errorf("delete path escapes root")
	}
	if _, err := os.Lstat(target); err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}

	trashRoot := deletionTrashDir(parentRoot)
	if err := ensureDir(trashRoot); err != nil {
		return "", true, err
	}

	var lastErr error
	for i := 0; i < 10; i++ {
		dst := filepath.Join(trashRoot, deletionTombstoneName(label, name))
		if i > 0 {
			dst = fmt.Sprintf("%s-%d", dst, i)
		}
		if err := os.Rename(target, dst); err == nil {
			return dst, true, nil
		} else {
			if os.IsNotExist(err) {
				return "", false, nil
			}
			lastErr = err
			time.Sleep(25 * time.Millisecond)
		}
	}
	return "", true, lastErr
}

func chmodTreeWritable(root string) {
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if info.IsDir() {
			_ = os.Chmod(p, 0o777)
			return nil
		}
		_ = os.Chmod(p, info.Mode().Perm()|0o600)
		return nil
	})
}

func removeAllWritable(root string) error {
	root = filepath.Clean(root)
	if root == "" || root == "." || root == string(os.PathSeparator) {
		return fmt.Errorf("refusing to remove unsafe path")
	}
	if err := os.RemoveAll(root); err == nil {
		return nil
	} else {
		firstErr := err
		chmodTreeWritable(root)
		if retryErr := os.RemoveAll(root); retryErr != nil {
			return fmt.Errorf("%v; retry after chmod failed: %w", firstErr, retryErr)
		}
	}
	return nil
}

func removeDetachedPathAsync(label, root string) {
	root = filepath.Clean(root)
	if root == "" || root == "." || root == string(os.PathSeparator) {
		return
	}
	go func() {
		started := time.Now()
		if err := removeAllWritable(root); err != nil {
			fmt.Printf("[delete] async cleanup failed for %s %s after %s: %v\n", label, root, time.Since(started).Round(time.Millisecond), err)
			return
		}
		fmt.Printf("[delete] async cleanup completed for %s %s in %s\n", label, root, time.Since(started).Round(time.Millisecond))
	}()
}

func (a *Agent) cleanupPendingDeletes() {
	roots := []string{deletionTrashDir(a.volumesDir), deletionTrashDir(a.backupsDir())}
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			removeDetachedPathAsync("startup-trash", filepath.Join(root, entry.Name()))
		}
	}
}

type BackupMeta struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	ServerName  string `json:"serverName"`
	CreatedAt   int64  `json:"createdAt"`
	Size        int64  `json:"size"`
	Status      string `json:"status"`
}

func (a *Agent) loadBackupMeta(backupDir string) *BackupMeta {
	metaPath := filepath.Join(backupDir, "backup.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return nil
	}
	var meta BackupMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil
	}
	return &meta
}

func (a *Agent) saveBackupMeta(backupDir string, meta *BackupMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return writeFileNoFollow(filepath.Join(backupDir, "backup.json"), data, 0644)
}

func getDirSize(path string) int64 {
	var size int64
	filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return size
}

func (a *Agent) handleBackupsList(w http.ResponseWriter, r *http.Request, serverName string) {
	backupsDir := a.serverBackupsDir(serverName)

	entries, err := os.ReadDir(backupsDir)
	if err != nil {
		if os.IsNotExist(err) {
			jsonWrite(w, 200, map[string]any{"ok": true, "backups": []any{}})
			return
		}
		jsonWrite(w, 500, map[string]any{"error": err.Error()})
		return
	}

	var backups []BackupMeta
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		backupDir := filepath.Join(backupsDir, entry.Name())
		meta := a.loadBackupMeta(backupDir)
		if meta != nil {
			backups = append(backups, *meta)
		}
	}

	for i := 0; i < len(backups)-1; i++ {
		for j := i + 1; j < len(backups); j++ {
			if backups[j].CreatedAt > backups[i].CreatedAt {
				backups[i], backups[j] = backups[j], backups[i]
			}
		}
	}

	jsonWrite(w, 200, map[string]any{"ok": true, "backups": backups})
}

func (a *Agent) handleBackupsCreate(w http.ResponseWriter, r *http.Request, serverName string) {
	serverDir := a.serverDir(serverName)
	if _, err := os.Stat(serverDir); err != nil {
		jsonWrite(w, 404, map[string]any{"error": "server not found"})
		return
	}
	if diskErr := a.checkDiskLimit(serverDir, 0); diskErr != nil {
		jsonWrite(w, 507, diskErr)
		return
	}

	meta := a.loadMeta(serverDir)
	if resources, ok := meta["resources"].(map[string]any); ok {
		if backupsMaxRaw, ok := resources["backupsMax"]; ok {
			var backupsMax int
			switch v := backupsMaxRaw.(type) {
			case float64:
				backupsMax = int(v)
			case int:
				backupsMax = v
			case int64:
				backupsMax = int(v)
			}

			if backupsMax > 0 {
				backupsDir := a.serverBackupsDir(serverName)
				entries, err := os.ReadDir(backupsDir)
				currentCount := 0
				if err == nil {
					for _, e := range entries {
						if e.IsDir() {
							currentCount++
						}
					}
				}
				if currentCount >= backupsMax {
					jsonWrite(w, 403, map[string]any{
						"error":   "backup_limit_reached",
						"message": fmt.Sprintf("Maximum backup limit reached (%d/%d). Delete old backups to create new ones.", currentCount, backupsMax),
						"current": currentCount,
						"max":     backupsMax,
					})
					return
				}
			}
		}
	}

	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	_ = readJSON(w, r, 1024*1024, &body)

	backupId := fmt.Sprintf("%d_%s", time.Now().UnixMilli(), randomHex(6))

	backupDir := filepath.Join(a.serverBackupsDir(serverName), backupId)
	if err := ensureDir(backupDir); err != nil {
		jsonWrite(w, 500, map[string]any{"error": "failed to create backup directory"})
		return
	}

	dataDir := filepath.Join(backupDir, "data")
	if err := ensureDir(dataDir); err != nil {
		jsonWrite(w, 500, map[string]any{"error": "failed to create data directory"})
		return
	}

	if err := copyDir(serverDir, dataDir); err != nil {
		os.RemoveAll(backupDir)
		jsonWrite(w, 500, map[string]any{"error": "failed to copy server files: " + err.Error()})
		return
	}

	size := getDirSize(dataDir)

	backupName := strings.TrimSpace(body.Name)
	if backupName == "" {
		backupName = fmt.Sprintf("Backup %s", time.Now().Format("2006-01-02 15:04"))
	}

	backupMeta := &BackupMeta{
		ID:          backupId,
		Name:        backupName,
		Description: strings.TrimSpace(body.Description),
		ServerName:  serverName,
		CreatedAt:   time.Now().UnixMilli(),
		Size:        size,
		Status:      "complete",
	}

	if err := a.saveBackupMeta(backupDir, backupMeta); err != nil {
		os.RemoveAll(backupDir)
		jsonWrite(w, 500, map[string]any{"error": "failed to save backup metadata"})
		return
	}

	a.logf("Created backup %s for server %s (size: %d bytes)", backupId, serverName, size)
	jsonWrite(w, 200, map[string]any{"ok": true, "backup": backupMeta})
}

func (a *Agent) handleSubdomains(w http.ResponseWriter, r *http.Request, serverName string) {
	var body struct {
		Domain string `json:"domain"`
		Action string `json:"action"`
		Port   int    `json:"port"`
	}
	if err := readJSON(w, r, 1024, &body); err != nil {
		jsonWrite(w, 400, map[string]any{"error": "bad json"})
		return
	}

	action := strings.ToLower(strings.TrimSpace(body.Action))
	if action != "create" && action != "delete" {
		jsonWrite(w, 400, map[string]any{"error": "action must be 'create' or 'delete'"})
		return
	}

	domain := strings.TrimSpace(body.Domain)
	if !isValidDomainName(domain) {
		jsonWrite(w, 400, map[string]any{"error": "invalid domain name"})
		return
	}

	safeDomain := sanitizeName(domain)
	if safeDomain == "" {
		jsonWrite(w, 400, map[string]any{"error": "invalid domain name"})
		return
	}
	confPath := filepath.Join("/etc/nginx/conf.d", fmt.Sprintf("%s-%s.conf", serverName, safeDomain))

	if action == "delete" {
		os.Remove(confPath)
		if err := exec.Command("nginx", "-t").Run(); err != nil {
			jsonWrite(w, 500, map[string]any{"error": "nginx config test failed after delete"})
			return
		}
		exec.Command("systemctl", "reload", "nginx").Run()
		jsonWrite(w, 200, map[string]any{"ok": true})
		return
	}

	serverDir := a.serverDir(serverName)
	meta := a.loadMeta(serverDir)

	allowedPort := anyToInt(meta["hostPort"])

	var targetPort int

	allocatedPorts := make(map[int]bool)
	if allowedPort > 0 {
		allocatedPorts[allowedPort] = true
	}
	forwards := a.loadPortForwards(serverDir)
	for _, pf := range forwards {
		if pf.Public > 0 {
			allocatedPorts[pf.Public] = true
		}
	}

	if len(allocatedPorts) == 0 {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "server has no allocated ports for subdomain proxy"})
		return
	}

	if body.Port > 0 {
		if !allocatedPorts[body.Port] {
			jsonWrite(w, 400, map[string]any{"ok": false, "error": "port is not allocated to this server"})
			return
		}
		targetPort = body.Port
	} else if allowedPort > 0 {
		targetPort = allowedPort
	} else if len(forwards) > 0 {
		targetPort = forwards[0].Public
	}

	if targetPort < 1 || targetPort > 65535 {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "no valid port available"})
		return
	}

	config := fmt.Sprintf(`server {
    listen 80;
    server_name %s;

    location / {
        proxy_pass http://127.0.0.1:%d;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}`, domain, targetPort)

	if err := writeFileNoFollow(confPath, []byte(config), 0644); err != nil {
		jsonWrite(w, 500, map[string]any{"error": "failed to write nginx config"})
		return
	}

	if err := exec.Command("nginx", "-t").Run(); err != nil {
		os.Remove(confPath)
		jsonWrite(w, 500, map[string]any{"error": "nginx config validation failed - config removed"})
		return
	}

	if err := exec.Command("systemctl", "reload", "nginx").Run(); err != nil {
		jsonWrite(w, 500, map[string]any{"error": "failed to reload nginx"})
		return
	}

	jsonWrite(w, 200, map[string]any{"ok": true})
}

func isValidDomainName(d string) bool {
	if len(d) == 0 || len(d) > 253 {
		return false
	}
	if strings.ContainsAny(d, "\"';{}$\\`()!@#%^&=+[]|<>?/ \t\n\r") {
		return false
	}
	labels := strings.Split(d, ".")
	if len(labels) == 0 {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i, c := range label {
			if c == '*' {
				if i != 0 || label != "*" {
					return false
				}
				continue
			}
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
				(c >= '0' && c <= '9') || c == '-') {
				return false
			}
		}
	}
	return true
}

func (a *Agent) handleBackupsRestore(w http.ResponseWriter, r *http.Request, serverName, backupId string) {
	if !validateBackupId(backupId) {
		jsonWrite(w, 400, map[string]any{"error": "invalid backup id format"})
		return
	}

	backupDir := filepath.Join(a.serverBackupsDir(serverName), backupId)
	dataDir := filepath.Join(backupDir, "data")

	if _, err := os.Stat(dataDir); err != nil {
		jsonWrite(w, 404, map[string]any{"error": "backup not found"})
		return
	}

	var body struct {
		DeleteOldFiles bool `json:"deleteOldFiles"`
	}
	_ = readJSON(w, r, 1024, &body)

	serverDir := a.serverDir(serverName)
	if !body.DeleteOldFiles {
		backupSize, _ := getDirSizeBytes(dataDir)
		if diskErr := a.checkDiskLimit(serverDir, backupSize); diskErr != nil {
			jsonWrite(w, 507, diskErr)
			return
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	a.docker.updateRestartNo(ctx, serverName)
	a.docker.stop(ctx, serverName)
	cancel()

	if body.DeleteOldFiles {
		entries, _ := os.ReadDir(serverDir)
		for _, entry := range entries {
			p := filepath.Join(serverDir, entry.Name())
			os.RemoveAll(p)
		}
	}

	if err := copyDir(dataDir, serverDir); err != nil {
		jsonWrite(w, 500, map[string]any{"error": "failed to restore backup: " + err.Error()})
		return
	}

	a.logf("Restored backup %s for server %s (deleteOldFiles=%v)", backupId, serverName, body.DeleteOldFiles)
	jsonWrite(w, 200, map[string]any{"ok": true})
}

func (a *Agent) handleBackupsDelete(w http.ResponseWriter, r *http.Request, serverName, backupId string) {
	if !validateBackupId(backupId) {
		jsonWrite(w, 400, map[string]any{"error": "invalid backup id format"})
		return
	}

	backupDir := filepath.Join(a.serverBackupsDir(serverName), backupId)

	if _, err := os.Stat(backupDir); err != nil {
		jsonWrite(w, 404, map[string]any{"error": "backup not found"})
		return
	}

	if err := os.RemoveAll(backupDir); err != nil {
		jsonWrite(w, 500, map[string]any{"error": "failed to delete backup"})
		return
	}

	a.logf("Deleted backup %s for server %s", backupId, serverName)
	jsonWrite(w, 200, map[string]any{"ok": true})
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func validateBackupId(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	return true
}

func copyDir(src, dst string) error {
	srcResolved, err := filepath.EvalSymlinks(src)
	if err != nil {
		return fmt.Errorf("failed to resolve source path: %w", err)
	}

	return filepath.Walk(srcResolved, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}

		linfo, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if linfo.Mode()&os.ModeSymlink != 0 {
			return nil
		}

		relPath, err := filepath.Rel(srcResolved, path)
		if err != nil {
			return err
		}
		destPath := filepath.Join(dst, relPath)

		if !isUnder(dst, destPath) {
			return fmt.Errorf("path traversal detected: %s", relPath)
		}

		if linfo.IsDir() {
			return os.MkdirAll(destPath, linfo.Mode())
		}

		srcFile, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			if errors.Is(err, syscall.ELOOP) {
				return nil
			}
			return err
		}
		defer srcFile.Close()

		srcStat, err := srcFile.Stat()
		if err != nil {
			return err
		}
		if !srcStat.Mode().IsRegular() {
			return nil
		}

		parent := filepath.Dir(destPath)
		resolvedParent, rpErr := filepath.EvalSymlinks(parent)
		if rpErr == nil && !isUnder(dst, resolvedParent) {
			return fmt.Errorf("symlink escape blocked: %s", relPath)
		}
		if di, lpErr := os.Lstat(destPath); lpErr == nil && (di.Mode()&os.ModeSymlink != 0) {
			return nil
		}
		dstFile, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, linfo.Mode())
		if err != nil {
			return err
		}
		defer dstFile.Close()

		if _, err := io.Copy(dstFile, srcFile); err != nil {
			return err
		}

		return os.Chmod(destPath, linfo.Mode())
	})
}

func (a *Agent) safeUnderVolumes(p string) (string, bool) {
	s := strings.TrimSpace(p)
	if s == "" {
		return "", false
	}
	if !filepath.IsAbs(s) {
		s = filepath.Join(a.volumesDir, s)
	}
	s = filepath.Clean(s)
	if !isUnder(a.volumesDir, s) && filepath.Clean(a.volumesDir) != s {
		return "", false
	}
	return s, true
}

func (a *Agent) handleFSList(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path  string `json:"path"`
		Depth int    `json:"depth"`
	}
	if err := readJSON(w, r, a.uploadLimit, &body); err != nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "bad json"})
		return
	}
	abs, ok := a.safeUnderVolumes(body.Path)
	if !ok {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid path"})
		return
	}
	st, err := os.Lstat(abs)
	if err != nil || !st.IsDir() {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "not a directory"})
		return
	}
	if err := ensureResolvedUnder(a.volumesDir, abs); err != nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "symlink escape blocked"})
		return
	}
	ents, err := os.ReadDir(abs)
	if err != nil {
		if a.debug {
			fmt.Printf("[fs] list error for %s: %v\n", abs, err)
		}
		jsonWrite(w, 500, map[string]any{"ok": false, "error": "failed to read directory"})
		return
	}
	out := make([]map[string]any, 0, len(ents))
	for _, e := range ents {
		entryPath := filepath.Join(abs, e.Name())
		linfo, lErr := os.Lstat(entryPath)
		if lErr != nil {
			continue
		}
		if linfo.Mode()&os.ModeSymlink != 0 {
			out = append(out, map[string]any{"name": e.Name(), "type": "symlink"})
			continue
		}
		typ := "file"
		if linfo.IsDir() {
			typ = "dir"
		}
		entry := map[string]any{"name": e.Name(), "type": typ}
		if !linfo.IsDir() {
			entry["size"] = linfo.Size()
		}
		out = append(out, entry)
	}
	jsonWrite(w, 200, map[string]any{"ok": true, "entries": out, "depth": body.Depth})
}

func (a *Agent) handleFSRead(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path     string `json:"path"`
		Encoding string `json:"encoding"`
	}
	if err := readJSON(w, r, 1<<20, &body); err != nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "bad json"})
		return
	}
	abs, ok := a.safeUnderVolumes(body.Path)
	if !ok {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid path"})
		return
	}
	info, err := os.Lstat(abs)
	if err != nil {
		jsonWrite(w, 404, map[string]any{"ok": false, "error": "file not found"})
		return
	}
	if info.Mode()&os.ModeSymlink != 0 {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "cannot read symlink"})
		return
	}
	if err := ensureResolvedUnder(a.volumesDir, abs); err != nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "symlink escape blocked"})
		return
	}
	if info.IsDir() {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "path is a directory"})
		return
	}
	const maxEditorRead = 10 << 20
	if info.Size() > maxEditorRead {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "file too large (max 10MB)"})
		return
	}
	rf, rfErr := os.OpenFile(abs, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if rfErr != nil {
		if errors.Is(rfErr, syscall.ELOOP) {
			jsonWrite(w, 400, map[string]any{"ok": false, "error": "cannot read symlink"})
			return
		}
		if a.debug {
			fmt.Printf("[fs] read error for %s: %v\n", abs, rfErr)
		}
		jsonWrite(w, 500, map[string]any{"ok": false, "error": "failed to read file"})
		return
	}
	defer rf.Close()
	if rfStat, sErr := rf.Stat(); sErr != nil || !rfStat.Mode().IsRegular() {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "not a regular file"})
		return
	}
	b, err := io.ReadAll(io.LimitReader(rf, maxEditorRead))
	if err != nil {
		if a.debug {
			fmt.Printf("[fs] read error for %s: %v\n", abs, err)
		}
		jsonWrite(w, 500, map[string]any{"ok": false, "error": "failed to read file"})
		return
	}
	enc := strings.ToLower(strings.TrimSpace(body.Encoding))
	if enc == "" {
		enc = "utf8"
	}
	content := ""
	if enc == "utf8" {
		content = string(b)
	} else {
		content = base64.StdEncoding.EncodeToString(b)
	}
	jsonWrite(w, 200, map[string]any{"ok": true, "content": content, "encoding": enc})
}

func (a *Agent) handleFSDownload(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := readJSON(w, r, 1<<20, &body); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	abs, ok := a.safeUnderVolumes(body.Path)
	if !ok {
		http.Error(w, "invalid path", 400)
		return
	}
	info, err := os.Lstat(abs)
	if err != nil {
		http.Error(w, "file not found", 404)
		return
	}
	if info.Mode()&os.ModeSymlink != 0 {
		http.Error(w, "cannot download symlink", 400)
		return
	}
	if err := ensureResolvedUnder(a.volumesDir, abs); err != nil {
		http.Error(w, "symlink escape blocked", 400)
		return
	}
	if info.IsDir() {
		http.Error(w, "path is a directory", 400)
		return
	}
	f, err := os.OpenFile(abs, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			http.Error(w, "cannot download symlink", 400)
			return
		}
		http.Error(w, "failed to open file", 500)
		return
	}
	defer f.Close()
	fStat, sErr := f.Stat()
	if sErr != nil || !fStat.Mode().IsRegular() {
		http.Error(w, "not a regular file", 400)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	cdName := strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < 0x20 || r == 0x7f {
			return '_'
		}
		return r
	}, filepath.Base(abs))
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, cdName))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", fStat.Size()))
	io.Copy(w, f)
}

func (a *Agent) handleFSWrite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path     string `json:"path"`
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := readJSON(w, r, 10<<20, &body); err != nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "bad json or content too large (max 10MB)"})
		return
	}
	abs, ok := a.safeUnderVolumes(body.Path)
	if !ok {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid path"})
		return
	}
	if diskErr := a.checkDiskLimitForPath(abs, int64(len(body.Content))); diskErr != nil {
		diskErr["ok"] = false
		jsonWrite(w, 507, diskErr)
		return
	}

	parentDir := filepath.Dir(abs)
	resolvedParent, err := filepath.EvalSymlinks(parentDir)
	if err == nil && !isUnder(a.volumesDir, resolvedParent) {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "symlink escape blocked"})
		return
	}

	_ = ensureDir(filepath.Dir(abs))
	if rp, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil && !isUnder(a.volumesDir, rp) {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "symlink escape blocked"})
		return
	}
	enc := strings.ToLower(strings.TrimSpace(body.Encoding))
	if enc == "" {
		enc = "utf8"
	}
	var data []byte
	if enc == "utf8" {
		data = []byte(body.Content)
	} else {
		b, err := base64.StdEncoding.DecodeString(body.Content)
		if err != nil {
			jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid base64"})
			return
		}
		data = b
	}
	if info, err := os.Lstat(abs); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			jsonWrite(w, 400, map[string]any{"ok": false, "error": "cannot write to symlink"})
			return
		}
	}
	if err := writeFileNoFollow(abs, data, 0o644); err != nil {
		if a.debug {
			fmt.Printf("[fs] write error for %s: %v\n", abs, err)
		}
		jsonWrite(w, 500, map[string]any{"ok": false, "error": "failed to write file"})
		return
	}
	jsonWrite(w, 200, map[string]any{"ok": true})
}

func (a *Agent) handleFSDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path  string `json:"path"`
		IsDir bool   `json:"isDir"`
	}
	if err := readJSON(w, r, 64*1024, &body); err != nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "bad json"})
		return
	}
	abs, ok := a.safeUnderVolumes(body.Path)
	if !ok {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid path"})
		return
	}
	if filepath.Clean(abs) == filepath.Clean(a.volumesDir) {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "refusing to delete volumes root"})
		return
	}

	info, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			jsonWrite(w, 200, map[string]any{"ok": true, "note": "already missing"})
			return
		}
		jsonWrite(w, 500, map[string]any{"ok": false, "error": "failed to access path"})
		return
	}

	if info.Mode()&os.ModeSymlink != 0 {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "cannot delete symlinks"})
		return
	}
	if err := ensureResolvedUnder(a.volumesDir, abs); err != nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "symlink escape blocked"})
		return
	}

	var delErr error
	if body.IsDir {
		delErr = os.RemoveAll(abs)
	} else {
		delErr = os.Remove(abs)
	}
	if delErr != nil {
		if a.debug {
			fmt.Printf("[fs] delete error for %s: %v\n", abs, delErr)
		}
		jsonWrite(w, 500, map[string]any{"ok": false, "error": "delete operation failed"})
		return
	}
	jsonWrite(w, 200, map[string]any{"ok": true})
}

func (a *Agent) handleFSDeleteBatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Items []struct {
			Path  string `json:"path"`
			IsDir bool   `json:"isDir"`
		} `json:"items"`
	}
	if err := readJSON(w, r, a.uploadLimit, &body); err != nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "bad json"})
		return
	}
	if len(body.Items) == 0 {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "no items to delete"})
		return
	}
	if len(body.Items) > 500 {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "too many items (max 500)"})
		return
	}

	results := make([]map[string]any, 0, len(body.Items))
	successCount := 0
	failCount := 0

	for _, item := range body.Items {
		abs, ok := a.safeUnderVolumes(item.Path)
		if !ok {
			results = append(results, map[string]any{"path": item.Path, "ok": false, "error": "invalid path"})
			failCount++
			continue
		}
		if filepath.Clean(abs) == filepath.Clean(a.volumesDir) {
			results = append(results, map[string]any{"path": item.Path, "ok": false, "error": "refusing to delete volumes root"})
			failCount++
			continue
		}
		linfo, err := os.Lstat(abs)
		if err != nil {
			results = append(results, map[string]any{"path": item.Path, "ok": true, "note": "already missing"})
			successCount++
			continue
		}
		if linfo.Mode()&os.ModeSymlink != 0 {
			results = append(results, map[string]any{"path": item.Path, "ok": false, "error": "cannot delete symlinks"})
			failCount++
			continue
		}
		if err := ensureResolvedUnder(a.volumesDir, abs); err != nil {
			results = append(results, map[string]any{"path": item.Path, "ok": false, "error": "symlink escape blocked"})
			failCount++
			continue
		}
		if recheck, rcErr := os.Lstat(abs); rcErr != nil {
			results = append(results, map[string]any{"path": item.Path, "ok": true, "note": "already missing"})
			successCount++
			continue
		} else if recheck.Mode()&os.ModeSymlink != 0 {
			results = append(results, map[string]any{"path": item.Path, "ok": false, "error": "cannot delete symlinks"})
			failCount++
			continue
		}
		var delErr error
		if item.IsDir {
			delErr = os.RemoveAll(abs)
		} else {
			delErr = os.Remove(abs)
		}
		if delErr != nil {
			if a.debug {
				fmt.Printf("[fs] batch delete error for %s: %v\n", item.Path, delErr)
			}
			results = append(results, map[string]any{"path": item.Path, "ok": false, "error": "delete failed"})
			failCount++
		} else {
			results = append(results, map[string]any{"path": item.Path, "ok": true})
			successCount++
		}
	}

	jsonWrite(w, 200, map[string]any{
		"ok":      failCount == 0,
		"results": results,
		"success": successCount,
		"failed":  failCount,
	})
}

func (a *Agent) handleFSRename(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path    string `json:"path"`
		NewName string `json:"newName"`
	}
	if err := readJSON(w, r, a.uploadLimit, &body); err != nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "bad json"})
		return
	}
	newName := sanitizeName(body.NewName)
	if newName == "" {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid newName"})
		return
	}
	abs, ok := a.safeUnderVolumes(body.Path)
	if !ok {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid path"})
		return
	}
	if err := ensureResolvedUnder(a.volumesDir, abs); err != nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "symlink escape blocked"})
		return
	}
	parent := filepath.Dir(abs)
	target := filepath.Join(parent, newName)
	if !isUnder(a.volumesDir, target) {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid target"})
		return
	}
	resolvedParent, rpErr := filepath.EvalSymlinks(parent)
	if rpErr == nil && !isUnder(a.volumesDir, resolvedParent) {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "symlink escape blocked"})
		return
	}
	if li, lErr := os.Lstat(abs); lErr == nil && li.Mode()&os.ModeSymlink != 0 {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "cannot rename symlinks"})
		return
	}
	if err := os.Rename(abs, target); err != nil {
		jsonWrite(w, 500, map[string]any{"ok": false, "error": "rename operation failed"})
		return
	}
	jsonWrite(w, 200, map[string]any{"ok": true})
}

func (a *Agent) handleFSExtract(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := readJSON(w, r, a.uploadLimit, &body); err != nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "bad json"})
		return
	}
	abs, ok := a.safeUnderVolumes(body.Path)
	if !ok {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid path"})
		return
	}
	if diskErr := a.checkDiskLimitForPath(abs, 0); diskErr != nil {
		diskErr["ok"] = false
		jsonWrite(w, 507, diskErr)
		return
	}
	if err := ensureResolvedUnder(a.volumesDir, abs); err != nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "symlink escape blocked"})
		return
	}
	resolvedDir, rdErr := filepath.EvalSymlinks(filepath.Dir(abs))
	if rdErr == nil && !isUnder(a.volumesDir, resolvedDir) {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "symlink escape blocked"})
		return
	}
	st, err := os.Stat(abs)
	if err != nil || st.IsDir() {
		jsonWrite(w, 404, map[string]any{"ok": false, "error": "file not found"})
		return
	}
	dir := filepath.Dir(abs)
	typ := detectArchiveType(abs)
	if typ == "" {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "unsupported archive type"})
		return
	}

	switch typ {
	case "zip":
		if err := extractZipSafe(abs, dir); err != nil {
			if a.debug {
				fmt.Printf("[fs] extract zip error for %s: %v\n", abs, err)
			}
			jsonWrite(w, 500, map[string]any{"ok": false, "error": "extract failed"})
			return
		}
	case "tar":
		f, err := os.Open(abs)
		if err != nil {
			jsonWrite(w, 500, map[string]any{"ok": false, "error": "failed to open archive"})
			return
		}
		defer f.Close()
		tr := tar.NewReader(f)
		if err := extractTarSafe(tr, dir); err != nil {
			if a.debug {
				fmt.Printf("[fs] extract tar error for %s: %v\n", abs, err)
			}
			jsonWrite(w, 500, map[string]any{"ok": false, "error": "extract failed"})
			return
		}
	case "targz":
		f, err := os.Open(abs)
		if err != nil {
			jsonWrite(w, 500, map[string]any{"ok": false, "error": "failed to open archive"})
			return
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			jsonWrite(w, 500, map[string]any{"ok": false, "error": "invalid gzip archive"})
			return
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		if err := extractTarSafe(tr, dir); err != nil {
			if a.debug {
				fmt.Printf("[fs] extract targz error for %s: %v\n", abs, err)
			}
			jsonWrite(w, 500, map[string]any{"ok": false, "error": "extract failed"})
			return
		}
	case "tarbz2":
		f, err := os.Open(abs)
		if err != nil {
			jsonWrite(w, 500, map[string]any{"ok": false, "error": "failed to open archive"})
			return
		}
		defer f.Close()
		br := bzip2.NewReader(f)
		tr := tar.NewReader(br)
		if err := extractTarSafe(tr, dir); err != nil {
			if a.debug {
				fmt.Printf("[fs] extract tarbz2 error for %s: %v\n", abs, err)
			}
			jsonWrite(w, 500, map[string]any{"ok": false, "error": "extract failed"})
			return
		}
	case "tarxz":
		if err := a.extractTarXzSafe(abs, dir); err != nil {
			if a.debug {
				fmt.Printf("[fs] extract tarxz error for %s: %v\n", abs, err)
			}
			jsonWrite(w, 500, map[string]any{"ok": false, "error": "extract failed"})
			return
		}
	case "7z":
		if err := a.extract7z(abs, dir); err != nil {
			if a.debug {
				fmt.Printf("[fs] extract 7z error for %s: %v\n", abs, err)
			}
			jsonWrite(w, 500, map[string]any{"ok": false, "error": "extract failed"})
			return
		}
	case "rar":
		if err := a.extractRar(abs, dir); err != nil {
			if a.debug {
				fmt.Printf("[fs] extract rar error for %s: %v\n", abs, err)
			}
			jsonWrite(w, 500, map[string]any{"ok": false, "error": "extract failed"})
			return
		}
	}

	_ = sanitizeExtractedTree(dir)

	jsonWrite(w, 200, map[string]any{"ok": true, "msg": "extracted"})
}

func (a *Agent) handleFSUploadRaw(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Dir      string `json:"dir"`
		Filename string `json:"filename"`
		DataB64  string `json:"data_b64"`
	}
	if err := readJSON(w, r, a.uploadLimit, &body); err != nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "bad json"})
		return
	}
	if strings.TrimSpace(body.Filename) == "" {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "missing filename"})
		return
	}
	absDir, ok := a.safeUnderVolumes(body.Dir)
	if !ok {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid dir"})
		return
	}
	if diskErr := a.checkDiskLimitForPath(absDir, int64(len(body.DataB64)*3/4)); diskErr != nil {
		diskErr["ok"] = false
		jsonWrite(w, 507, diskErr)
		return
	}
	_ = ensureDir(absDir)
	if err := ensureResolvedUnder(a.volumesDir, absDir); err != nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "symlink escape blocked"})
		return
	}

	fn := filepath.Base(body.Filename)
	fn = strings.NewReplacer("\r", "_", "\n", "_", "\x00", "_").Replace(fn)
	if fn == "" || fn == "." || fn == ".." {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid filename"})
		return
	}
	dest := filepath.Join(absDir, fn)
	if !isUnder(absDir, dest) {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid filename"})
		return
	}

	const maxBase64Upload = 25 << 20
	if base64.StdEncoding.DecodedLen(len(body.DataB64)) > maxBase64Upload {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "file too large for base64 upload; use stream upload"})
		return
	}
	data, err := base64.StdEncoding.DecodeString(body.DataB64)
	if err != nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid base64"})
		return
	}
	if info, err := os.Lstat(dest); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			jsonWrite(w, 400, map[string]any{"ok": false, "error": "cannot write to symlink"})
			return
		}
	}
	if err := writeFileNoFollow(dest, data, 0o644); err != nil {
		if a.debug {
			fmt.Printf("[fs] upload raw error for %s: %v\n", dest, err)
		}
		jsonWrite(w, 500, map[string]any{"ok": false, "error": "failed to write file"})
		return
	}
	jsonWrite(w, 200, map[string]any{"ok": true, "path": dest})
}

func (a *Agent) handleFSUploadStream(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, a.uploadLimit)

	mr, err := r.MultipartReader()
	if err != nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "bad multipart"})
		return
	}

	var absDir string
	var destPath string
	var uploaded bool
	var written int64

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			jsonWrite(w, 400, map[string]any{"ok": false, "error": "bad multipart"})
			return
		}

		formName := part.FormName()
		fileName := part.FileName()

		if formName == "dir" {
			b, readErr := io.ReadAll(io.LimitReader(part, 64<<10))
			if readErr != nil {
				_ = part.Close()
				jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid dir"})
				return
			}
			_ = part.Close()
			if len(b) >= 64<<10 {
				jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid dir"})
				return
			}
			parsedDir, ok := a.safeUnderVolumes(string(b))
			if !ok {
				jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid dir"})
				return
			}
			if err := ensureResolvedUnder(a.volumesDir, parsedDir); err != nil {
				jsonWrite(w, 400, map[string]any{"ok": false, "error": "symlink escape blocked"})
				return
			}
			absDir = parsedDir
			continue
		}

		if formName != "file" || fileName == "" {
			_, _ = io.Copy(io.Discard, part)
			_ = part.Close()
			continue
		}

		if uploaded {
			_, _ = io.Copy(io.Discard, part)
			_ = part.Close()
			jsonWrite(w, 400, map[string]any{"ok": false, "error": "too many files"})
			return
		}
		if absDir == "" {
			_, _ = io.Copy(io.Discard, part)
			_ = part.Close()
			jsonWrite(w, 400, map[string]any{"ok": false, "error": "dir field must be sent before file"})
			return
		}

		rawName := filepath.Base(fileName)
		safeName := sanitizeName(rawName)
		if safeName == "" {
			safeName = "upload.bin"
		}

		_ = ensureDir(absDir)
		destPath = filepath.Join(absDir, safeName)

		var replacedBytes int64
		if info, err := os.Lstat(destPath); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				_ = part.Close()
				jsonWrite(w, 400, map[string]any{"ok": false, "error": "cannot overwrite symlink"})
				return
			}
			if info.IsDir() {
				_ = part.Close()
				jsonWrite(w, 400, map[string]any{"ok": false, "error": "destination is a directory"})
				return
			}
			replacedBytes = info.Size()
		}

		allowedBytes, baseUsedBytes, limitBytes, diskLimited, diskErr := a.uploadDiskAllowanceForPath(absDir, replacedBytes)
		if diskErr != nil {
			_ = part.Close()
			diskErr["ok"] = false
			jsonWrite(w, 507, diskErr)
			return
		}

		out, err := os.OpenFile(destPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW, 0o644)
		if err != nil {
			_ = part.Close()
			if a.debug {
				fmt.Printf("[fs] upload stream create error: %v\n", err)
			}
			jsonWrite(w, 500, map[string]any{"ok": false, "error": "file create failed"})
			return
		}

		diskAllowedBytes := int64(-1)
		if diskLimited {
			diskAllowedBytes = allowedBytes
		}
		written, limitKind, err := copyUploadWithLimits(out, part, a.uploadLimit, diskAllowedBytes)
		closeErr := out.Close()
		_ = part.Close()
		if err != nil {
			_ = os.Remove(destPath)
			if a.debug {
				fmt.Printf("[fs] upload stream write error: %v\n", err)
			}
			jsonWrite(w, 500, map[string]any{"ok": false, "error": "write failed"})
			return
		}
		if closeErr != nil {
			_ = os.Remove(destPath)
			jsonWrite(w, 500, map[string]any{"ok": false, "error": "close failed"})
			return
		}
		if limitKind == "upload" {
			_ = os.Remove(destPath)
			jsonWrite(w, 413, map[string]any{
				"ok":       false,
				"error":    "file too large",
				"limit_mb": a.uploadLimit / (1024 * 1024),
			})
			return
		}
		if limitKind == "disk" {
			_ = os.Remove(destPath)
			out := diskLimitUploadPayload(baseUsedBytes, limitBytes, written+1)
			out["ok"] = false
			jsonWrite(w, 507, out)
			return
		}

		uploaded = true
	}

	if !uploaded {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "missing file field"})
		return
	}

	jsonWrite(w, 200, map[string]any{"ok": true, "path": destPath, "bytes": written})
}

func uploadURLAllowsAnyPort() bool {
	return os.Getenv("ADPANEL_NODE_FETCH_ALLOW_ANY_PORT") == "1" || os.Getenv("REMOTE_FETCH_ALLOW_ANY_PORT") == "1"
}

func uploadURLPortAllowed(u *url.URL) bool {
	if uploadURLAllowsAnyPort() {
		return true
	}
	port := u.Port()
	return port == "" || port == "80" || port == "443"
}

func sanitizeUploadFilenameForURL(name string) string {
	s := strings.TrimSpace(name)
	if s == "" {
		return ""
	}
	if decoded, err := url.PathUnescape(s); err == nil {
		s = decoded
	}
	s = strings.NewReplacer("\x00", "_", "\r", "_", "\n", "_", "\\", "/").Replace(s)
	s = path.Base(s)
	if s == "." || s == ".." || s == "/" || s == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '-', r == '_', r == ' ':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.TrimSpace(strings.TrimLeft(b.String(), "."))
	if out == "" || out == "." || out == ".." {
		return ""
	}
	if len(out) > 200 {
		out = out[len(out)-200:]
		out = strings.TrimLeft(out, ". ")
	}
	if out == "" || out == "." || out == ".." {
		return ""
	}
	return out
}

func filenameFromContentDisposition(cd string) string {
	if strings.TrimSpace(cd) == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(cd)
	if err != nil {
		return ""
	}
	return params["filename"]
}

func openURLUploadTempFile(dir, filename string) (*os.File, string, error) {
	prefix := "." + sanitizeUploadFilenameForURL(filename)
	if prefix == "." {
		prefix = ".download"
	}
	for i := 0; i < 16; i++ {
		tmpPath := filepath.Join(dir, fmt.Sprintf("%s.%s.urlupload.tmp", prefix, randomHex(8)))
		if !isUnder(dir, tmpPath) {
			return nil, "", fmt.Errorf("invalid temp path")
		}
		f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o644)
		if err == nil {
			return f, tmpPath, nil
		}
		if os.IsExist(err) {
			continue
		}
		return nil, "", err
	}
	return nil, "", fmt.Errorf("could not allocate temp file")
}

func (a *Agent) handleFSUploadURL(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Dir string `json:"dir"`
		URL string `json:"url"`
	}
	if err := readJSON(w, r, 16<<10, &body); err != nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "bad json"})
		return
	}

	rawURL := strings.TrimSpace(body.URL)
	if rawURL == "" {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "missing url"})
		return
	}
	if len(rawURL) > 2048 {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "url too long"})
		return
	}

	parsedURL, err := a.dl.safeURL(rawURL)
	if err != nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid url", "detail": err.Error()})
		return
	}
	if !uploadURLPortAllowed(parsedURL) {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid url", "detail": "only ports 80/443 are allowed"})
		return
	}

	absDir, ok := a.safeUnderVolumes(body.Dir)
	if !ok {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid dir"})
		return
	}
	dirInfo, err := os.Lstat(absDir)
	if err != nil || !dirInfo.IsDir() {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "target dir not found"})
		return
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "cannot upload into symlink"})
		return
	}
	if err := ensureResolvedUnder(a.volumesDir, absDir); err != nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "symlink escape blocked"})
		return
	}
	if diskErr := a.checkDiskLimitForPath(absDir, 0); diskErr != nil {
		diskErr["ok"] = false
		jsonWrite(w, 507, diskErr)
		return
	}

	maxBytes := a.uploadLimit
	if maxBytes <= 0 {
		maxBytes = 1024 * 1024 * 1024
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), nil)
	if err != nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid url"})
		return
	}
	req.Header.Set("User-Agent", "ADPanel-Node/"+NodeVersion)
	req.Header.Set("Accept", "*/*")

	client := a.dl.clientNoProxy()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("too many redirects")
		}
		nextURL, err := a.dl.safeURL(req.URL.String())
		if err != nil {
			return err
		}
		if !uploadURLPortAllowed(nextURL) {
			return fmt.Errorf("only ports 80/443 are allowed")
		}
		return nil
	}

	resp, err := client.Do(req)
	if err != nil {
		if a.debug {
			fmt.Printf("[fs] uploadUrl request error: %v\n", err)
		}
		jsonWrite(w, 502, map[string]any{"ok": false, "error": "fetch failed", "detail": "remote request failed"})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		jsonWrite(w, 502, map[string]any{"ok": false, "error": "fetch failed", "detail": fmt.Sprintf("remote http %d", resp.StatusCode)})
		return
	}

	if resp.ContentLength > maxBytes {
		jsonWrite(w, 413, map[string]any{
			"ok":       false,
			"error":    "file too large",
			"limit_mb": maxBytes / (1024 * 1024),
		})
		return
	}

	filename := sanitizeUploadFilenameForURL(filenameFromContentDisposition(resp.Header.Get("Content-Disposition")))
	if filename == "" && parsedURL.Path != "" {
		filename = sanitizeUploadFilenameForURL(path.Base(parsedURL.Path))
	}
	if filename == "" {
		filename = "download.bin"
	}

	destPath := filepath.Join(absDir, filename)
	if !isUnder(absDir, destPath) {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid filename"})
		return
	}

	var replacedBytes int64
	if info, err := os.Lstat(destPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			jsonWrite(w, 400, map[string]any{"ok": false, "error": "cannot overwrite symlink"})
			return
		}
		if info.IsDir() {
			jsonWrite(w, 400, map[string]any{"ok": false, "error": "destination is a directory"})
			return
		}
		replacedBytes = info.Size()
	}

	if resp.ContentLength > 0 {
		extraBytes := resp.ContentLength - replacedBytes
		if extraBytes < 0 {
			extraBytes = 0
		}
		if diskErr := a.checkDiskLimitForPath(absDir, extraBytes); diskErr != nil {
			diskErr["ok"] = false
			jsonWrite(w, 507, diskErr)
			return
		}
	}

	allowedBytes, baseUsedBytes, limitBytes, diskLimited, diskErr := a.uploadDiskAllowanceForPath(absDir, replacedBytes)
	if diskErr != nil {
		diskErr["ok"] = false
		jsonWrite(w, 507, diskErr)
		return
	}
	if resp.ContentLength > 0 && diskLimited && resp.ContentLength > allowedBytes {
		out := diskLimitUploadPayload(baseUsedBytes, limitBytes, resp.ContentLength)
		out["ok"] = false
		jsonWrite(w, 507, out)
		return
	}

	out, tmpPath, err := openURLUploadTempFile(absDir, filename)
	if err != nil {
		if a.debug {
			fmt.Printf("[fs] uploadUrl create error: %v\n", err)
		}
		jsonWrite(w, 500, map[string]any{"ok": false, "error": "file create failed"})
		return
	}
	committed := false
	defer func() {
		if !committed && tmpPath != "" {
			_ = os.Remove(tmpPath)
		}
	}()

	diskAllowedBytes := int64(-1)
	if diskLimited {
		diskAllowedBytes = allowedBytes
	}
	written, limitKind, copyErr := copyUploadWithLimits(out, resp.Body, maxBytes, diskAllowedBytes)
	closeErr := out.Close()
	if copyErr != nil {
		if a.debug {
			fmt.Printf("[fs] uploadUrl write error: %v\n", copyErr)
		}
		jsonWrite(w, 500, map[string]any{"ok": false, "error": "write failed"})
		return
	}
	if closeErr != nil {
		jsonWrite(w, 500, map[string]any{"ok": false, "error": "close failed"})
		return
	}
	if limitKind == "upload" {
		jsonWrite(w, 413, map[string]any{
			"ok":       false,
			"error":    "file too large",
			"limit_mb": maxBytes / (1024 * 1024),
		})
		return
	}
	if limitKind == "disk" {
		out := diskLimitUploadPayload(baseUsedBytes, limitBytes, written+1)
		out["ok"] = false
		jsonWrite(w, 507, out)
		return
	}

	if diskErr := a.checkDiskLimitForPathAfterReplacing(absDir, replacedBytes); diskErr != nil {
		diskErr["ok"] = false
		jsonWrite(w, 507, diskErr)
		return
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		if a.debug {
			fmt.Printf("[fs] uploadUrl rename error: %v\n", err)
		}
		jsonWrite(w, 500, map[string]any{"ok": false, "error": "rename failed"})
		return
	}
	committed = true

	jsonWrite(w, 200, map[string]any{
		"ok":       true,
		"path":     destPath,
		"filename": filename,
		"bytes":    written,
		"msg":      fmt.Sprintf("Downloaded %s (%d bytes)", filename, written),
	})
}

func main() {
	cfgPath := os.Getenv("ADPANEL_NODE_CONFIG")
	if strings.TrimSpace(cfgPath) == "" {
		cfgPath = DefaultConfigPath
	}
	if _, err := os.Stat(cfgPath); err != nil {
		fmt.Fprintf(os.Stderr, "[config] Missing config.yml at: %s\n", cfgPath)
		fmt.Fprintln(os.Stderr, "[config] This agent does NOT generate a config file.")
		os.Exit(1)
	}
	raw := string(mustReadFile(cfgPath))
	m, err := parseYAMLSubset(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[config] Failed to parse YAML: %v\n", err)
		os.Exit(1)
	}
	cfg := Config{Raw: m}

	debug := cfg.Bool("debug")
	token := cfg.String("token")
	tokenID := cfg.String("token_id")
	uuid := cfg.String("uuid")

	apiHost := cfg.String("api", "host")
	if apiHost == "" {
		apiHost = DefaultAPIHost
	}
	apiPort := cfg.Int("api", "port")
	if apiPort == 0 {
		apiPort = DefaultAPIPort
	}
	uploadMB := cfg.Int("api", "upload_limit")
	if uploadMB == 0 {
		uploadMB = 1024
	}
	uploadLimit := int64(uploadMB) * 1024 * 1024

	dataRoot := cfg.String("system", "data")
	if dataRoot == "" {
		dataRoot = DefaultDataRoot
	}

	oldVolumesDir := filepath.Join(dataRoot, "volumes")
	volumesDir := filepath.Join(dataRoot, "servers")

	if _, err := os.Stat(oldVolumesDir); err == nil {
		if _, err := os.Stat(volumesDir); os.IsNotExist(err) {
			fmt.Printf("[migration] Moving server data from %s to %s\n", oldVolumesDir, volumesDir)
			if err := os.Rename(oldVolumesDir, volumesDir); err != nil {
				fmt.Printf("[migration] Warning: could not migrate volumes: %v\n", err)
				volumesDir = oldVolumesDir
			} else {
				fmt.Printf("[migration] Successfully migrated server data\n")
			}
		}
	}

	_ = ensureDir(volumesDir)

	sftpPort := cfg.Int("system", "sftp", "bind_port")
	if sftpPort == 0 {
		sftpPort = DefaultSFTPPort
	}

	stopGraceMS := 1500
	if v := os.Getenv("STOP_GRACE_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 500 && n <= 30000 {
			stopGraceMS = n
		}
	}

	headerTimeoutMS := 30000
	if v := os.Getenv("HTTP_TIMEOUT_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1000 {
			headerTimeoutMS = n
		}
	}

	downloadMax := int64(10) << 30
	if v := os.Getenv("DOWNLOAD_MAX_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			downloadMax = n
		}
	}

	resourceStatsTTL := serverResourceStatsDefaultTTL
	if v := os.Getenv("RESOURCE_STATS_CACHE_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 500 && n <= 30000 {
			resourceStatsTTL = time.Duration(n) * time.Millisecond
		}
	}
	resourceStatsDiskTTL := serverResourceStatsDiskTTL
	if v := os.Getenv("RESOURCE_STATS_DISK_CACHE_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1000 && n <= 300000 {
			resourceStatsDiskTTL = time.Duration(n) * time.Millisecond
		}
	}

	panelHMAC := strings.TrimSpace(os.Getenv("PANEL_HMAC_SECRET"))
	if panelHMAC == "" {
		panelHMAC = strings.TrimSpace(cfg.String("panel", "hmac_secret"))
	}

	agent := &Agent{
		cfg:         cfg,
		debug:       debug,
		token:       token,
		tokenID:     tokenID,
		uuid:        uuid,
		apiHost:     apiHost,
		apiPort:     apiPort,
		uploadLimit: uploadLimit,
		dataRoot:    dataRoot,
		volumesDir:  volumesDir,
		sftpPort:    sftpPort,
		panelHMAC:   panelHMAC,
		stopGrace:   time.Duration(stopGraceMS) * time.Millisecond,
		docker:      Docker{debug: debug},
		dl: Downloader{
			headerTimeout: time.Duration(headerTimeoutMS) * time.Millisecond,
			maxBytes:      downloadMax,
		},
		resourceStats:  newServerResourceStatsCache(resourceStatsTTL, resourceStatsDiskTTL),
		stoppingNow:    make(map[string]time.Time),
		recentCommands: make(map[string]recentConsoleCommand),
		importProgress: make(map[string]*ImportProgress),
		importSem:      make(chan struct{}, 3),
	}
	agent.ensureNodeInformationFile()
	agent.cleanupPendingDeletes()
	agent.logs = newLogManager(agent.docker, debug)
	agent.stdinMgr = newStdinManager(&agent.docker, debug)

	protectNodeProcess()

	startRateLimitCleanup()

	diskMon := newDiskMonitor(agent, 60*time.Second, debug)
	diskMon.start()

	memMon := newMemoryMonitor(agent, 10*time.Second, debug)
	memMon.start()

	networkAbuseMon := newNetworkAbuseMonitor(agent, 5*time.Second, debug)
	networkAbuseMon.start()

	statsCacheStop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				agent.resourceStats.cleanup(serverResourceStatsMaxIdle)
			case <-statsCacheStop:
				return
			}
		}
	}()

	httpSrv := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", agent.apiHost, agent.apiPort),
		Handler:           agent,
		ReadTimeout:       60 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	banner := strings.Join([]string{
		"ADPanel Node Agent",
		fmt.Sprintf("UUID: %s", func() string {
			if agent.uuid != "" {
				return agent.uuid
			}
			return "n/a"
		}()),
		fmt.Sprintf("Version: %s", NodeVersion),
		"SFTP functionality is not implemented yet on this preview build.",
		fmt.Sprintf("Use HTTP API on port %d.", agent.apiPort),
		"",
	}, "\n")

	sftpLn, err := net.Listen("tcp", fmt.Sprintf("%s:%d", agent.apiHost, agent.sftpPort))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[sftp-port] listen error: %v\n", err)
		os.Exit(1)
	}
	go func() {
		for {
			c, err := sftpLn.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
				_, _ = conn.Write([]byte(banner + "\n"))
			}(c)
		}
	}()

	fmt.Printf("[node] API listening on %s://%s:%d\n", func() string {
		if cfg.Bool("api", "ssl", "enabled") {
			return "https"
		}
		return "http"
	}(), agent.apiHost, agent.apiPort)
	fmt.Printf("[node] SFTP placeholder listening on %s:%d\n", agent.apiHost, agent.sftpPort)

	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sig
		fmt.Println("[node] shutting down...")

		memMon.stop()

		diskMon.stop()

		networkAbuseMon.stop()

		close(statsCacheStop)

		if agent.stdinMgr != nil {
			agent.stdinMgr.stop()
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
		_ = sftpLn.Close()
	}()

	sslEnabled := cfg.Bool("api", "ssl", "enabled")
	cert := cfg.String("api", "ssl", "cert")
	key := cfg.String("api", "ssl", "key")

	if envCert := os.Getenv("API_TLS_CERT"); envCert != "" {
		cert = envCert
	}
	if envKey := os.Getenv("API_TLS_KEY"); envKey != "" {
		key = envKey
	}

	if sslEnabled && cert != "" && key != "" {
		if _, err := os.Stat(cert); err != nil {
			fmt.Fprintf(os.Stderr, "[ssl] Certificate file not found: %s — falling back to HTTP\n", cert)
			sslEnabled = false
		}
		if sslEnabled {
			if _, err := os.Stat(key); err != nil {
				fmt.Fprintf(os.Stderr, "[ssl] Key file not found: %s — falling back to HTTP\n", key)
				sslEnabled = false
			}
		}
	} else if sslEnabled && (cert == "" || key == "") {
		fmt.Fprintln(os.Stderr, "[ssl] SSL enabled but cert/key paths not provided — falling back to HTTP")
		sslEnabled = false
	}

	if sslEnabled {
		fmt.Printf("[node] Starting HTTPS with cert=%s key=%s\n", cert, key)
		if err := httpSrv.ListenAndServeTLS(cert, key); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "[https] error: %v\n", err)
			os.Exit(1)
		}
	} else {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "[http] error: %v\n", err)
			os.Exit(1)
		}
	}
}
