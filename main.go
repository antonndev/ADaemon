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
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)


const (
	NodeVersion       = "1.2.1"
	DefaultConfigPath = "/var/lib/node/config.yml"
	DefaultDataRoot   = "/var/lib/node"
	DefaultAPIHost    = "0.0.0.0"
	DefaultAPIPort    = 8080
	DefaultSFTPPort   = 2022
)

type Config struct {
	Raw map[string]any
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

func (d Docker) rmForce(ctx context.Context, name string) {
	_, _, _, _ = d.runCollect(ctx, "rm", "-f", name)
}

func (d Docker) pull(ctx context.Context, ref string) {
	_, _, _, _ = d.runCollect(ctx, "pull", ref)
}

func (d Docker) imageExistsLocally(ctx context.Context, ref string) bool {
	_, _, exitCode, _ := d.runCollect(ctx, "image", "inspect", ref)
	return exitCode == 0
}

type imageConfig struct {
	Volumes []string
	Workdir string
	User    string
	Env     map[string]string
}

func (d Docker) inspectImageConfig(ctx context.Context, ref string) imageConfig {
	cfg := imageConfig{Env: map[string]string{}}

	out, _, code, err := d.runCollect(ctx, "inspect", "--format",
		`{{json .Config.Volumes}}||{{.Config.WorkingDir}}||{{.Config.User}}||{{json .Config.Env}}`, ref)
	if err != nil || code != 0 {
		return cfg
	}

	parts := strings.SplitN(strings.TrimSpace(out), "||", 4)
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

	return cfg
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
	entries, err := os.ReadDir(serverDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		child := filepath.Join(serverDir, e.Name())
		if e.IsDir() {
			_ = os.Chmod(child, 0777)
		} else {
			_ = os.Chmod(child, 0666)
		}
	}
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

func (d Docker) stop(ctx context.Context, name string) {
	_, _, _, _ = d.runCollect(ctx, "stop", "-t", "15", name)
}


const (
	stdinMaxContainers = 100
	stdinIdleTimeout   = 10 * time.Minute
	stdinWriteTimeout  = 5 * time.Second
	stdinCommandMaxLen = 4096
	stdinBufferSize    = 256
)

type stdinConn struct {
	name     string
	stdin    io.WriteCloser
	cmd      *exec.Cmd
	cmdCh    chan string
	closeCh  chan struct{}
	lastUsed time.Time
	mu       sync.Mutex
	closed   bool
}

type stdinManager struct {
	mu       sync.RWMutex
	conns    map[string]*stdinConn
	docker   *Docker
	debug    bool
	stopCh   chan struct{}
	stopOnce sync.Once
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
	name := strings.TrimSpace(containerName)
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
	cmd := exec.Command("docker", "attach", "--no-stdout", "--no-stderr", "--sig-proxy=false", name)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdin pipe: %w", err)
	}

	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		stdin.Close()
		return nil, fmt.Errorf("failed to start docker attach: %w", err)
	}

	conn := &stdinConn{
		name:     name,
		stdin:    stdin,
		cmd:      cmd,
		cmdCh:    make(chan string, stdinBufferSize),
		closeCh:  make(chan struct{}),
		lastUsed: time.Now(),
	}

	go conn.writerLoop(sm.debug)

	go func() {
		_ = cmd.Wait()
		conn.mu.Lock()
		conn.closed = true
		conn.mu.Unlock()
		close(conn.closeCh)
	}()

	if sm.debug {
		fmt.Printf("[stdin-manager] created new connection for %s\n", name)
	}

	return conn, nil
}

func (c *stdinConn) writerLoop(debug bool) {
	for {
		select {
		case <-c.closeCh:
			return
		case cmd := <-c.cmdCh:
			c.mu.Lock()
			if c.closed {
				c.mu.Unlock()
				return
			}
			c.lastUsed = time.Now()
			c.mu.Unlock()

			_, err := fmt.Fprintln(c.stdin, cmd)
			if err != nil {
				if debug {
					fmt.Printf("[stdin-conn] write error for %s: %v\n", c.name, err)
				}
				c.close()
				return
			}
		}
	}
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
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("connection closed")
	}
	c.mu.Unlock()

	select {
	case c.cmdCh <- cmd:
		return nil
	default:
		return fmt.Errorf("command queue full")
	}
}

func (sm *stdinManager) remove(name string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if conn, ok := sm.conns[name]; ok {
		conn.close()
		delete(sm.conns, name)
	}
}


func (d Docker) sendCommand(ctx context.Context, stdinMgr *stdinManager, containerName, command string, isMinecraft bool) error {
	name := strings.TrimSpace(containerName)
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

	if !d.containerExists(ctx, name) {
		return fmt.Errorf("container-not-running")
	}

	if isMinecraft {
		pipeCmd := exec.CommandContext(ctx, "docker", "exec", "-i", name, "sh", "-c", "cat > /tmp/minecraft-console-in")
		pipeCmd.Stdin = strings.NewReader(cmd + "\n")
		if err := pipeCmd.Run(); err == nil {
			if d.debug {
				fmt.Printf("[docker] minecraft command sent via console-in pipe\n")
			}
			return nil
		}

		execCmd := exec.CommandContext(ctx, "docker", "exec", name, "mc-send-to-console", cmd)
		if err := execCmd.Run(); err == nil {
			if d.debug {
				fmt.Printf("[docker] minecraft command sent via mc-send-to-console\n")
			}
			return nil
		}

		execCmd = exec.CommandContext(ctx, "docker", "exec", name, "rcon-cli", cmd)
		if err := execCmd.Run(); err == nil {
			if d.debug {
				fmt.Printf("[docker] minecraft command sent via rcon-cli\n")
			}
			return nil
		}
	}

	if stdinMgr != nil {
		conn, err := stdinMgr.getOrCreate(ctx, name)
		if err == nil {
			if err := conn.sendCommand(cmd); err == nil {
				if d.debug {
					fmt.Printf("[docker] command sent via stdin manager\n")
				}
				return nil
			}
			stdinMgr.remove(name)
		}
	}

	execCtx, cancel := context.WithTimeout(ctx, stdinWriteTimeout)
	defer cancel()

	execCmd := exec.CommandContext(execCtx, "docker", "exec", "-i", name, "cat")
	execCmd.Stdin = strings.NewReader(cmd + "\n")
	if err := execCmd.Run(); err == nil {
		if d.debug {
			fmt.Printf("[docker] command sent via docker exec stdin\n")
		}
		return nil
	}

	return fmt.Errorf("failed-to-send: all methods exhausted")
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
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("missing host")
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("dns failed: %w", err)
	}
	for _, ip := range ips {
		addr, ok := netip.AddrFromSlice(ip)
		if !ok {
			continue
		}
		if !addr.IsGlobalUnicast() {
			return nil, fmt.Errorf("blocked host ip: %s", addr.String())
		}
	}
	return u, nil
}

func isPrivateIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	return !addr.IsGlobalUnicast()
}

func (dl Downloader) client() *http.Client {
	safeDialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
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


const pythonMainTemplate = `def greet(name="World"):
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

func clampPort(p any, fallback int) int {
	switch v := p.(type) {
	case float64:
		p = int(v)
	case int:
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(v))
		p = n
	}
	n, ok := p.(int)
	if !ok {
		return fallback
	}
	if n < 1 || n > 65535 {
		return fallback
	}
	return n
}

func buildNodeIndexTemplate(port int) string {
	p := port
	if p < 1 || p > 65535 {
		p = 3001
	}
	return fmt.Sprintf(`const express = require("express");
const app = express();

app.get("/", (req, res) => {
  res.send("Hello World from ADPanel!");
});

const PORT = %d;

app.listen(PORT, () => {
  console.log(`+"`"+`Server running on http://localhost:%d`+"`"+`);
});
`, p, p)
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
	_ = os.WriteFile(filepath.Join(serverDir, sf), []byte(pythonMainTemplate), 0o644)
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
	_ = os.WriteFile(filepath.Join(serverDir, sf), []byte(buildNodeIndexTemplate(port)), 0o644)

	pkgPath := filepath.Join(serverDir, "package.json")
	if _, err := os.Stat(pkgPath); err == nil {
		return
	}

	pkg := map[string]any{
		"name":    filepath.Base(serverDir),
		"private": true,
		"version": "1.0.0",
		"main":    "index.js",
		"scripts": map[string]string{"start": "node index.js"},
		"dependencies": map[string]string{
			"express": "^4.19.2",
		},
	}
	b, _ := json.MarshalIndent(pkg, "", "  ")
	_ = os.WriteFile(pkgPath, b, 0o644)
}


var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
var mcHexRE = regexp.MustCompile(`§x(?:§[0-9A-Fa-f]){6}`)
var mcRE = regexp.MustCompile(`§[0-9A-FK-ORa-fk-or]`)

func cleanLogLine(s string) string {
	if s == "" {
		return s
	}
	s = ansiRE.ReplaceAllString(s, "")
	s = mcHexRE.ReplaceAllString(s, "")
	s = mcRE.ReplaceAllString(s, "")
	return s
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

	stopMu      sync.Mutex
	stoppingNow map[string]time.Time

	importProgressMu sync.RWMutex
	importProgress   map[string]*ImportProgress

	importSem chan struct{}

	wsMu          sync.Mutex
	wsSubscribers map[*wsSubscriber]struct{}

	metaLocks sync.Map

	serverOpLocks sync.Map
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
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
	})
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

const cpuPeriod = 100000

func computeCpuLimit(allocatedCores float64) (effectiveCores, maxPercent float64) {
	hostCores := float64(runtime.NumCPU())
	if allocatedCores > 0 {
		effectiveCores = allocatedCores
		if effectiveCores > hostCores {
			effectiveCores = hostCores
		}
		maxPercent = effectiveCores * 100
	} else {
		effectiveCores = hostCores * 0.85
		maxPercent = effectiveCores * 100
	}
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

func (a *Agent) startMinecraftContainer(ctx context.Context, name, serverDir string, hostPort int, resources *resourceLimits, extraEnv map[string]string) error {
	a.docker.rmForce(ctx, name)
	a.docker.pull(ctx, "itzg/minecraft-server:latest")
	ensureVolumeWritable(serverDir)
	uid, gid := statUIDGID(serverDir)

	args := []string{
		"run", "-d", "-t",
		"--name", name,
		"--restart", "unless-stopped",
		"--cap-drop=ALL",
	}
	args = append(args, panelSafeCaps()...)
	args = append(args,
		"--pids-limit=512",
		"-p", fmt.Sprintf("%d:25565", hostPort),
	)
	portForwards := a.loadPortForwards(serverDir)
	for _, pf := range portForwards {
		containerPort := 25565
		args = append(args, "-p", fmt.Sprintf("%d:%d", pf.Public, containerPort))
		fmt.Printf("[port-forwards] %s: forwarding public %d -> container %d\n", name, pf.Public, containerPort)
	}
	if mcMeta := a.loadMeta(serverDir); mcMeta != nil {
		if metaRes, ok := mcMeta["resources"].(map[string]any); ok {
			if ports, ok := metaRes["ports"].([]any); ok {
				for _, p := range ports {
					ap := anyToInt(p)
					if ap > 0 && ap != hostPort {
						args = append(args, "-p", fmt.Sprintf("%d:%d", ap, ap))
						fmt.Printf("[port-manage] %s: exposing additional port %d:%d\n", name, ap, ap)
					}
				}
			}
		}
	}
	args = append(args,
		"-v", fmt.Sprintf("%s:/data", serverDir),
		"-e", "EULA=TRUE",
		"-e", "TYPE=CUSTOM",
		"-e", "CUSTOM_SERVER=/data/server.jar",
		"-e", "ENABLE_RCON=false",
		"-e", "CREATE_CONSOLE_IN_PIPE=true",
		"-e", fmt.Sprintf("UID=%d", uid),
		"-e", fmt.Sprintf("GID=%d", gid),
	)

	for k, v := range extraEnv {
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
	}

	if resources != nil {
		if resources.RamMb != nil && *resources.RamMb > 0 {
			args = append(args, "--memory", fmt.Sprintf("%dm", *resources.RamMb))
			args = append(args, "--memory-reservation", fmt.Sprintf("%dm", *resources.RamMb*90/100))

			if resources.SwapMb == nil {
				args = append(args, "--memory-swap", fmt.Sprintf("%dm", *resources.RamMb))
			} else {
				swapVal := *resources.SwapMb
				if swapVal == -1 {
					args = append(args, "--memory-swap", fmt.Sprintf("%dm", *resources.RamMb))
				} else if swapVal == 0 {
					args = append(args, "--memory-swap", "-1")
				} else {
					totalSwap := *resources.RamMb + swapVal
					args = append(args, "--memory-swap", fmt.Sprintf("%dm", totalSwap))
				}
			}
		}
		if resources.CpuCores != nil && *resources.CpuCores > 0 && *resources.CpuCores <= 128 {
			effCores, _ := computeCpuLimit(*resources.CpuCores)
			cpuQuota := int64(float64(cpuPeriod) * effCores)
			args = append(args, "--cpu-period", fmt.Sprintf("%d", cpuPeriod))
			args = append(args, "--cpu-quota", fmt.Sprintf("%d", cpuQuota))
		} else {
			effCores, _ := computeCpuLimit(0)
			cpuQuota := int64(float64(cpuPeriod) * effCores)
			args = append(args, "--cpu-period", fmt.Sprintf("%d", cpuPeriod))
			args = append(args, "--cpu-quota", fmt.Sprintf("%d", cpuQuota))
		}
	}

	args = append(args, "itzg/minecraft-server:latest")
	_, errStr, _, err := a.docker.runCollect(ctx, args...)
	if err != nil {
		if a.debug {
			fmt.Printf("[docker] minecraft container error: %s\n", strings.TrimSpace(errStr))
		}
		return fmt.Errorf("container start failed")
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
	isNode := tpl == "nodejs" || tpl == "discord-bot"

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
	a.docker.pull(ctx, imageRef)
	ensureVolumeWritable(serverDir)

	effectivePort := 0
	if hostPort > 0 {
		effectivePort = hostPort
	}

	vols := []string{fmt.Sprintf("%s:/app", serverDir)}
	if vv, ok := runtimeObj["volumes"].([]any); ok && len(vv) > 0 {
		vols = vols[:0]
		for _, v := range vv {
			s := strings.TrimSpace(fmt.Sprint(v))
			if s == "" {
				continue
			}
			parts := strings.SplitN(s, ":", 2)
			if len(parts) >= 2 {
				hostPath := filepath.Clean(parts[0])
				if err := ensureResolvedUnder(a.volumesDir, hostPath); err != nil {
					fmt.Printf("[security] blocked volume mount outside volumes dir: %s\n", s)
					continue
				}
			}
			vols = append(vols, s)
		}
		if len(vols) == 0 {
			vols = []string{fmt.Sprintf("%s:/app", serverDir)}
		}
	}

	env := map[string]string{}
	if ev, ok := runtimeObj["env"].(map[string]any); ok {
		for k, v := range ev {
			env[k] = fmt.Sprint(v)
		}
	}
	for k, v := range extraEnv {
		env[k] = v
	}
	if effectivePort > 0 {
		if _, ok := env["PORT"]; !ok {
			env["PORT"] = fmt.Sprint(effectivePort)
		}
	}
	uid, gid := statUIDGID(serverDir)
	if _, ok := env["UID"]; !ok {
		env["UID"] = fmt.Sprint(uid)
	}
	if _, ok := env["GID"]; !ok {
		env["GID"] = fmt.Sprint(gid)
	}

	args := []string{"run", "-d", "-t", "--name", name, "--restart", "unless-stopped"}
	args = append(args, containerSecurityArgs(uid, gid, false)...)

	if resources != nil {
		if resources.RamMb != nil && *resources.RamMb > 0 {
			args = append(args, "--memory", fmt.Sprintf("%dm", *resources.RamMb))
			args = append(args, "--memory-reservation", fmt.Sprintf("%dm", *resources.RamMb*90/100))

			if resources.SwapMb == nil {
				args = append(args, "--memory-swap", fmt.Sprintf("%dm", *resources.RamMb))
			} else {
				swapVal := *resources.SwapMb
				if swapVal == -1 {
					args = append(args, "--memory-swap", fmt.Sprintf("%dm", *resources.RamMb))
				} else if swapVal == 0 {
					args = append(args, "--memory-swap", "-1")
				} else {
					totalSwap := *resources.RamMb + swapVal
					args = append(args, "--memory-swap", fmt.Sprintf("%dm", totalSwap))
				}
			}
		}
		if resources.CpuCores != nil && *resources.CpuCores > 0 && *resources.CpuCores <= 128 {
			effCores, _ := computeCpuLimit(*resources.CpuCores)
			cpuQuota := int64(float64(cpuPeriod) * effCores)
			args = append(args, "--cpu-period", fmt.Sprintf("%d", cpuPeriod))
			args = append(args, "--cpu-quota", fmt.Sprintf("%d", cpuQuota))
		} else {
			effCores, _ := computeCpuLimit(0)
			cpuQuota := int64(float64(cpuPeriod) * effCores)
			args = append(args, "--cpu-period", fmt.Sprintf("%d", cpuPeriod))
			args = append(args, "--cpu-quota", fmt.Sprintf("%d", cpuQuota))
		}
	}

	if effectivePort > 0 {
		args = append(args, "-p", fmt.Sprintf("%d:%d", effectivePort, effectivePort))
	}
	portForwards := a.loadPortForwards(serverDir)
	for _, pf := range portForwards {
		args = append(args, "-p", fmt.Sprintf("%d:%d", pf.Public, pf.Internal))
	}
	if resources != nil {
		if metaRes, ok := meta["resources"].(map[string]any); ok {
			if ports, ok := metaRes["ports"].([]any); ok {
				for _, p := range ports {
					ap := anyToInt(p)
					if ap > 0 && ap != effectivePort {
						args = append(args, "-p", fmt.Sprintf("%d:%d", ap, ap))
						fmt.Printf("[port-manage] %s: exposing additional port %d:%d\n", name, ap, ap)
					}
				}
			}
		}
	}
	for _, v := range vols {
		args = append(args, "-v", v)
	}
	for k, v := range env {
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
	}
	args = append(args, imageRef)

	startFile := strings.TrimSpace(fmt.Sprint(meta["start"]))
	if startFile == "" {
		if isPython {
			startFile = "main.py"
		} else {
			startFile = "index.js"
		}
	}

	defaultCmd := ""
	if isPython {
		defaultCmd = fmt.Sprintf("python /app/%s", startFile)
	} else if isNode {
		defaultCmd = fmt.Sprintf(`sh -c "cd /app && npm install && node /app/%s"`, startFile)
	}
	cmd := strings.TrimSpace(fmt.Sprint(runtimeObj["command"]))
	if cmd == "" {
		cmd = defaultCmd
	}
	if cmd != "" {
		args = append(args, "sh", "-lc", cmd)
	}

	_, errStr, _, err := a.docker.runCollect(ctx, args...)
	if err != nil {
		if a.debug {
			fmt.Printf("[docker] runtime container error: %s\n", strings.TrimSpace(errStr))
		}
		return fmt.Errorf("container start failed")
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

func (a *Agent) executeStartupCommand(ctx context.Context, name, serverDir string, hostPort int, startupCmd string, resources *resourceLimits, meta map[string]any) error {
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

	cmd = strings.TrimSpace(cmd)
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
		"--memory-swap": true, "--cpu-period": true, "--cpu-quota": true,
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

	finalArgs := []string{"run", "-d", "-t", "--restart", "unless-stopped", "--name", name}
	finalArgs = append(finalArgs, "--cap-drop=ALL", "--pids-limit=512")
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
		} else {
			effCores, _ := computeCpuLimit(0)
			cpuQuota := int64(float64(cpuPeriod) * effCores)
			finalArgs = append(finalArgs, "--cpu-period", fmt.Sprintf("%d", cpuPeriod))
			finalArgs = append(finalArgs, "--cpu-quota", fmt.Sprintf("%d", cpuQuota))
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
		if ports, ok := metaRes["ports"].([]any); ok {
			for _, p := range ports {
				ap := anyToInt(p)
				if ap > 0 && ap != hostPort {
					finalArgs = append(finalArgs, "-p", fmt.Sprintf("%d:%d", ap, ap))
					fmt.Printf("[port-manage] %s: exposing additional port %d:%d\n", name, ap, ap)
				}
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
	allowedSet := make(map[int]bool)
	if allocatedPort > 0 {
		allowedSet[allocatedPort] = true
	}
	for _, p := range additionalAllocatedPorts {
		if p > 0 {
			allowedSet[p] = true
		}
	}
	reservedSet := make(map[int]bool, len(reservedPorts))
	for _, p := range reservedPorts {
		if p > 0 {
			reservedSet[p] = true
		}
	}
	cmd := strings.TrimSpace(startupCmd)
	cmdLower := strings.ToLower(cmd)
	if !strings.HasPrefix(cmdLower, "docker run") {
		return ""
	}
	argsStr := strings.TrimSpace(cmd[len("docker run"):])
	args := parseShellArgs(argsStr)

	skipNext := false
	for i := 0; i < len(args); i++ {
		if skipNext {
			skipNext = false
			continue
		}
		arg := args[i]
		argLower := strings.ToLower(strings.TrimSpace(arg))

		if (arg == "-p" || arg == "--publish") && i+1 < len(args) {
			portMapping := args[i+1]
			skipNext = true
			if strings.Contains(portMapping, "{PORT}") {
				continue
			}
			hp := extractHostPortFromMapping(portMapping)
			if hp > 0 && !allowedSet[hp] {
				return "Port config through docker edit command is not allowed. Add or remove a port for this server by the category 'Port Management'"
			}
			if hp > 0 && reservedSet[hp] {
				return fmt.Sprintf("Port %d in the docker command conflicts with a port forwarding rule.", hp)
			}
			continue
		}

		if strings.HasPrefix(arg, "-p=") || strings.HasPrefix(arg, "--publish=") {
			portVal := ""
			if strings.HasPrefix(arg, "-p=") {
				portVal = strings.TrimPrefix(arg, "-p=")
			} else {
				portVal = strings.TrimPrefix(arg, "--publish=")
			}
			if strings.Contains(portVal, "{PORT}") {
				continue
			}
			hp := extractHostPortFromMapping(portVal)
			if hp > 0 && !allowedSet[hp] {
				return "Port config through docker edit command is not allowed. Add or remove a port for this server by the category 'Port Management'"
			}
			if hp > 0 && reservedSet[hp] {
				return fmt.Sprintf("Port %d in the docker command conflicts with a port forwarding rule.", hp)
			}
			continue
		}

		if strings.HasPrefix(argLower, "-") {
			if strings.Contains(argLower, "=") {
				continue
			}
			portFlagsWithValue := map[string]bool{
				"-e": true, "--env": true, "-v": true, "--volume": true,
				"-p": true, "--publish": true, "-w": true, "--workdir": true,
				"--name": true, "-m": true, "--memory": true, "--cpus": true,
				"--restart": true, "-u": true, "--user": true, "-h": true, "--hostname": true,
				"--network": true, "--net": true, "-l": true, "--label": true,
				"--entrypoint": true,
			}
			if portFlagsWithValue[argLower] {
				skipNext = true
			}
			continue
		}
		break
	}
	return ""
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
		"--memory-swap": true, "--cpu-period": true, "--cpu-quota": true,
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
	if a.debug {
		fmt.Printf("[docker] startCustomDockerContainer for %s\n", name)
	}

	startupCmd := ""
	if cmd, ok := runtimeObj["startupCommand"].(string); ok && strings.TrimSpace(cmd) != "" {
		startupCmd = cmd
	} else if cmd, ok := meta["startupCommand"].(string); ok && strings.TrimSpace(cmd) != "" {
		startupCmd = cmd
	}

	if strings.TrimSpace(startupCmd) != "" {
		fmt.Printf("[docker] Found startupCommand: %s\n", startupCmd)
		return a.executeStartupCommand(ctx, name, serverDir, hostPort, startupCmd, resources, meta)
	}

	fmt.Printf("[docker] No startupCommand found, using image-based start\n")

	image := strings.TrimSpace(fmt.Sprint(runtimeObj["image"]))
	tag := strings.TrimSpace(fmt.Sprint(runtimeObj["tag"]))
	if image == "" {
		return fmt.Errorf("no docker image specified")
	}
	if tag == "" {
		tag = "latest"
	}
	imageRef := image + ":" + tag
	if !a.docker.imageExistsLocally(ctx, imageRef) {
		a.docker.pull(ctx, imageRef)
	}

	currentDigest := a.docker.getImageDigest(ctx, imageRef)
	lastDigest, _ := meta["lastImageDigest"].(string)

	volumeContainerPaths := []string{}
	if vv, ok := runtimeObj["volumes"].([]any); ok {
		for _, v := range vv {
			s := strings.TrimSpace(fmt.Sprint(v))
			parts := strings.Split(s, ":")
			if len(parts) >= 2 {
				containerPath := parts[1]
				if idx := strings.Index(containerPath, ":"); idx > 0 {
					containerPath = containerPath[:idx]
				}
				volumeContainerPaths = append(volumeContainerPaths, containerPath)
			}
		}
	}

	isGameServer := isGameServerDockerImage(image)

	ensureVolumeWritable(serverDir)

	if isGameServer {
		fmt.Printf("[docker] game server image detected (%s), skipping file extraction - container will download files\n", image)
	}


	if currentDigest != "" && currentDigest != lastDigest {
		_ = a.modifyMeta(serverDir, func(m map[string]any) {
			m["lastImageDigest"] = currentDigest
		})
	}


	vols := []string{}
	hasCustomVolumes := false
	if vv, ok := runtimeObj["volumes"].([]any); ok && len(vv) > 0 {
		for _, v := range vv {
			s := strings.TrimSpace(fmt.Sprint(v))
			if s != "" {
				s = strings.ReplaceAll(s, "{BOT_DIR}", serverDir)
				s = strings.ReplaceAll(s, "{SERVER_DIR}", serverDir)


				parts := strings.SplitN(s, ":", 2)
				if len(parts) >= 2 {
					hostPath := filepath.Clean(parts[0])
					if err := ensureResolvedUnder(a.volumesDir, hostPath); err != nil {
						fmt.Printf("[security] blocked volume mount outside volumes dir: %s\n", s)
						continue
					}
				}

				vols = append(vols, s)
				hasCustomVolumes = true
			}
		}
	}

	if !hasCustomVolumes {
		imageRef := image + ":" + tag
		imgCfg := a.docker.inspectImageConfig(ctx, imageRef)
		if len(imgCfg.Volumes) > 0 {
			for _, p := range imgCfg.Volumes {
				vols = append(vols, fmt.Sprintf("%s:%s", serverDir, p))
				fmt.Printf("[docker] auto-detected image VOLUME: %s -> %s\n", serverDir, p)
			}
		} else if envPaths := getDataPathsFromEnv(imgCfg.Env); len(envPaths) > 0 {
			vols = append(vols, fmt.Sprintf("%s:%s", serverDir, envPaths[0]))
			fmt.Printf("[docker] auto-detected data path from image ENV: %s -> %s\n", serverDir, envPaths[0])
		} else if imgCfg.Workdir != "" && imgCfg.Workdir != "/" {
			vols = append(vols, fmt.Sprintf("%s:%s", serverDir, imgCfg.Workdir))
			fmt.Printf("[docker] auto-detected image WORKDIR: %s -> %s\n", serverDir, imgCfg.Workdir)
		} else {
			if isGameServer {
				vols = append(vols, fmt.Sprintf("%s:/data", serverDir))
				fmt.Printf("[docker] no VOLUME/WORKDIR/ENV detected for game server, falling back to /data\n")
			} else {
				vols = append(vols, fmt.Sprintf("%s:/app", serverDir))
			}
		}
	}

	env := map[string]string{}
	if ev, ok := runtimeObj["env"].(map[string]any); ok {
		for k, v := range ev {
			env[k] = fmt.Sprint(v)
		}
	}

	if !isGameServer {
		if hostPort > 0 {
			if _, ok := env["PORT"]; !ok {
				env["PORT"] = fmt.Sprint(hostPort)
			}
		}
		uid, gid := statUIDGID(serverDir)
		if _, ok := env["UID"]; !ok {
			env["UID"] = fmt.Sprint(uid)
		}
		if _, ok := env["GID"]; !ok {
			env["GID"] = fmt.Sprint(gid)
		}
	}

	args := []string{"run", "-d"}
	if isGameServer {
		args = append(args, "-i", "-t")
	} else {
		args = append(args, "-t")
	}
	args = append(args, "--name", name)

	if isGameServer {
		args = append(args, "--restart", "on-failure:3")
	} else {
		args = append(args, "--restart", "unless-stopped")
	}

	args = append(args, "--cap-drop=ALL", "--pids-limit=512")
	args = append(args, panelSafeCaps()...)

	if !isGameServer {
		args = append(args, "-w", "/app")
	}

	if resources != nil {
		if resources.RamMb != nil && *resources.RamMb > 0 {
			args = append(args, "--memory", fmt.Sprintf("%dm", *resources.RamMb))
			args = append(args, "--memory-reservation", fmt.Sprintf("%dm", *resources.RamMb*90/100))
			if resources.SwapMb == nil {
				args = append(args, "--memory-swap", fmt.Sprintf("%dm", *resources.RamMb))
			} else {
				swapVal := *resources.SwapMb
				if swapVal == -1 {
					args = append(args, "--memory-swap", fmt.Sprintf("%dm", *resources.RamMb))
				} else if swapVal == 0 {
					args = append(args, "--memory-swap", "-1")
				} else {
					totalSwap := *resources.RamMb + swapVal
					args = append(args, "--memory-swap", fmt.Sprintf("%dm", totalSwap))
				}
			}
		}
		if resources.CpuCores != nil && *resources.CpuCores > 0 && *resources.CpuCores <= 128 {
			cpuQuota := int64(float64(cpuPeriod) * *resources.CpuCores)
			args = append(args, "--cpu-period", fmt.Sprintf("%d", cpuPeriod))
			args = append(args, "--cpu-quota", fmt.Sprintf("%d", cpuQuota))
		}
	}

	defaultContainerPort := hostPort
	if ports, ok := runtimeObj["ports"].([]any); ok && len(ports) > 0 {
		switch v := ports[0].(type) {
		case float64:
			if int(v) > 0 {
				defaultContainerPort = int(v)
			}
		case int:
			if v > 0 {
				defaultContainerPort = v
			}
		case string:
			if p, _ := strconv.Atoi(v); p > 0 {
				defaultContainerPort = p
			}
		}
	}

	if hostPort > 0 {
		args = append(args, "-p", fmt.Sprintf("%d:%d", hostPort, defaultContainerPort))
		fmt.Printf("[port-enforce] %s: only exposing allocated port %d -> container %d (template secondary ports stripped)\n", name, hostPort, defaultContainerPort)
	}
	portForwards := a.loadPortForwards(serverDir)
	for _, pf := range portForwards {
		containerPort := defaultContainerPort
		if containerPort <= 0 {
			containerPort = pf.Internal
		}
		args = append(args, "-p", fmt.Sprintf("%d:%d", pf.Public, containerPort))
		fmt.Printf("[port-forwards] %s: forwarding public %d -> container %d\n", name, pf.Public, containerPort)
	}

	for _, v := range vols {
		args = append(args, "-v", v)
	}

	for k, v := range env {
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
	}

	args = append(args, imageRef)

	cmd := ""
	if cmdVal := runtimeObj["command"]; cmdVal != nil {
		cmd = strings.TrimSpace(fmt.Sprint(cmdVal))
		if cmd == "<nil>" {
			cmd = ""
		}
	}

	isGameServerImage := isGameServerDockerImage(image)

	if cmd == "" && !isGameServerImage {
		cmd = guessStartupCommand(serverDir, image)
	}

	if cmd != "" && cmd != "<nil>" {
		if strings.ContainsAny(cmd, "&|;$`") || strings.Contains(cmd, "&&") || strings.Contains(cmd, "||") {
			args = append(args, "sh", "-c", cmd)
		} else {
			parts := strings.Fields(cmd)
			if len(parts) > 0 {
				args = append(args, parts...)
			}
		}
	} else if isGameServerImage {
		fmt.Printf("[docker] using image's default CMD for game server image: %s\n", image)
	}

	if a.debug {
		fmt.Printf("[docker] running: docker %s\n", strings.Join(args, " "))
	}
	_, errStr, exitCode, err := a.docker.runCollect(ctx, args...)
	if err != nil || exitCode != 0 {
		errMsg := strings.TrimSpace(errStr)
		fmt.Printf("[docker] custom container error (exit %d): %s (go err: %v)\n", exitCode, errMsg, err)
		if errMsg == "" && err != nil {
			errMsg = err.Error()
		}
		return fmt.Errorf("container start failed: %s", errMsg)
	}

	go a.recoverMisplacedFiles(name, serverDir)

	return nil
}

func isGameServerDockerImage(image string) bool {
	imageLower := strings.ToLower(image)
	gameServerPatterns := []string{
		"csgo", "cs2", "counterstrike",
		"minecraft", "paper", "spigot", "bukkit", "forge", "fabric",
		"valheim", "rust", "ark", "terraria",
		"fivem", "ragemp", "gtav", "gta5",
		"factorio", "satisfactory",
		"teamspeak", "ts3", "mumble",
		"starbound", "unturned", "7daystodie", "7dtd",
		"conan", "empyrion", "hurtworld",
		"mordhau", "squad", "insurgency",
		"garrysmod", "gmod", "srcds",
		"avorion", "astroneer", "barotrauma",
		"palworld", "enshrouded", "soulmask",
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
			return "cd /app && npm install && node index.js"
		}
		if _, err := os.Stat(filepath.Join(serverDir, "app.js")); err == nil {
			return "cd /app && npm install && node app.js"
		}
		return "cd /app && npm install && node index.js"
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
	name   string
	closed bool

	mu      sync.Mutex
	clients map[chan string]struct{}

	stopCh chan struct{}
	cmd    *exec.Cmd
}

type logManager struct {
	mu    sync.Mutex
	tails map[string]*logTail
	d     Docker
	debug bool
}

func newLogManager(d Docker, debug bool) *logManager {
	return &logManager{
		tails: map[string]*logTail{},
		d:     d,
		debug: debug,
	}
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
		t.mu.Unlock()

		close(t.stopCh)
		if t.cmd != nil && t.cmd.Process != nil {
			_ = t.cmd.Process.Kill()
		}
		delete(lm.tails, t.name)
	}
}

func (lm *logManager) followLoop(t *logTail) {
	backoff := 1 * time.Second
	lastExists := false
	fmt.Printf("[logs] starting follow loop for container: %s\n", t.name)
	for {
		select {
		case <-t.stopCh:
			fmt.Printf("[logs] stop signal received for: %s\n", t.name)
			return
		default:
		}

		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		exists := lm.d.containerExists(ctx, t.name)
		cancel()
		if !exists {
			if lastExists {
				fmt.Printf("[logs] container %s no longer exists, waiting...\n", t.name)
			}
			lastExists = false
			time.Sleep(backoff)
			if backoff < 5*time.Second {
				backoff += 500 * time.Millisecond
			}
			continue
		}

		if !lastExists {
			fmt.Printf("[logs] container %s found, attaching to logs\n", t.name)
		}
		lastExists = true
		backoff = 1 * time.Second

		cmd := exec.Command("docker", "logs", "--tail", "200", "-f", t.name)
		stdout, _ := cmd.StdoutPipe()
		stderr, _ := cmd.StderrPipe()
		if err := cmd.Start(); err != nil {
			fmt.Printf("[logs] failed to start docker logs for %s: %v\n", t.name, err)
			time.Sleep(1 * time.Second)
			continue
		}

		t.mu.Lock()
		t.cmd = cmd
		t.mu.Unlock()

		broadcast := func(prefix string, r io.Reader) {
			sc := bufio.NewScanner(r)
			buf := make([]byte, 0, 64*1024)
			sc.Buffer(buf, 2*1024*1024)
			for sc.Scan() {
				line := sc.Text()
				if line == "" {
					continue
				}
				if prefix == "stderr:" {
					if strings.Contains(strings.ToLower(line), "no such container") {
						continue
					}
					if strings.Contains(strings.ToLower(line), "can not get logs from container which is dead") {
						continue
					}
				}
				lm.sendToClients(t, prefix+line)
			}
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); broadcast("stdout:", stdout) }()
		go func() { defer wg.Done(); broadcast("stderr:", stderr) }()
		wg.Wait()

		_ = cmd.Wait()

		t.mu.Lock()
		t.cmd = nil
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
	defer t.mu.Unlock()
	for ch := range t.clients {
		select {
		case ch <- s:
		default:
		}
	}
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
	if t.cmd != nil && t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
	t.mu.Unlock()

	if !alreadyClosed {
		close(t.stopCh)
	}
	delete(lm.tails, name)
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
			"error":     "disk_limit_exceeded",
			"message":   fmt.Sprintf("This server has exceeded its disk space limit. Used: %.1f MB / %.1f MB", usedMb, limitMb),
			"usedBytes": usedBytes,
			"limitBytes": limitBytes,
			"usedMb":    usedMb,
			"limitMb":   limitMb,
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
				"dataRoot":   a.dataRoot,
				"volumesDir": a.volumesDir,
			},
		})
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
	RamMb        *int     `json:"ramMb"`
	CpuCores     *float64 `json:"cpuCores"`
	StorageMb    *int     `json:"storageMb"`
	StorageGb    *int     `json:"storageGb"`
	SwapMb       *int     `json:"swapMb"`
	BackupsMax   *int     `json:"backupsMax"`
	MaxSchedules *int     `json:"maxSchedules"`
}

type dockerTemplateConfig struct {
	Image          string            `json:"image"`
	Tag            string            `json:"tag"`
	Command        string            `json:"command"`
	Ports          []int             `json:"ports"`
	Volumes        []string          `json:"volumes"`
	Env            map[string]string `json:"env"`
	Restart        string            `json:"restart"`
	RestartPolicy  string            `json:"restartPolicy"`
	StartupCommand string            `json:"startupCommand"`
	Console        any               `json:"console"`
}

type createReq struct {
	Name           string                `json:"name"`
	Template       string                `json:"templateId"`
	MCFork         string                `json:"mcFork"`
	MCVersion      string                `json:"mcVersion"`
	HostPort       int                   `json:"hostPort"`
	AutoStart      bool                  `json:"autoStart"`
	Resources      *resourceLimits       `json:"resources"`
	Docker         *dockerTemplateConfig `json:"docker"`
	StartupCommand string                `json:"startupCommand"`
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

		mcImage := "itzg/minecraft-server"
		mcTag := "latest"
		if req.Docker != nil && req.Docker.Image != "" {
			mcImage = req.Docker.Image
			if req.Docker.Tag != "" {
				mcTag = req.Docker.Tag
			}
		}

		meta = map[string]any{
			"type":     "minecraft",
			"fork":     fork,
			"version":  ver,
			"start":    "server.jar",
			"hostPort": hostPort,
			"runtime": map[string]any{
				"image": mcImage,
				"tag":   mcTag,
			},
		}
		if len(resourcesMeta) > 0 {
			meta["resources"] = resourcesMeta
		}
		_ = a.saveMeta(serverDir, meta)

		enforceServerProps(serverDir)

		if req.ImportUrl != "" {
			fmt.Printf("[createServer] Importing archive from URL for Minecraft server: %s\n", req.ImportUrl)
			if err := a.downloadAndExtractArchiveSync(name, serverDir, req.ImportUrl); err != nil {
				fmt.Printf("[createServer] Warning: archive import failed: %v\n", err)
			}
		}

		effectiveCmd := a.buildEffectiveDockerCommand(name, serverDir, meta)
		if effectiveCmd != "" {
			meta["startupCommand"] = effectiveCmd
			runtimeObj, _ := meta["runtime"].(map[string]any)
			if runtimeObj == nil {
				runtimeObj = make(map[string]any)
			}
			runtimeObj["startupCommand"] = effectiveCmd
			meta["runtime"] = runtimeObj
			_ = a.saveMeta(serverDir, meta)
		}

		if req.AutoStart {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			var err error
			if effectiveCmd != "" {
				err = a.executeStartupCommand(ctx, name, serverDir, hostPort, effectiveCmd, req.Resources, meta)
			} else {
				err = a.startMinecraftContainer(ctx, name, serverDir, hostPort, req.Resources, nil)
			}
			cancel()
			if err != nil {
				fmt.Printf("[createServer] Container startup failed, cleaning up: %s - %v\n", name, err)
				_ = os.RemoveAll(serverDir)
				jsonWrite(w, 500, map[string]any{"error": "failed to start server", "detail": err.Error()})
				return
			}
		}
	} else {
		hostPort := req.HostPort
		if hostPort == 0 {
			hostPort = 8080
		}

		startupCmd := ""
		if req.StartupCommand != "" {
			startupCmd = req.StartupCommand
		} else if req.Docker != nil && req.Docker.StartupCommand != "" {
			startupCmd = req.Docker.StartupCommand
		}

		if startupCmd != "" && req.Docker != nil && req.Docker.Image != "" {
			cmdImage := extractDockerImageFromCommand(startupCmd)
			if cmdImage != "" {
				normalizedTemplate := normalizeDockerImage(req.Docker.Image)
				normalizedCmd := normalizeDockerImage(cmdImage)
				if normalizedTemplate != normalizedCmd {
					fmt.Printf("[security] BLOCKED startup command image mismatch at creation for %s: template=%q command=%q\n", name, req.Docker.Image, cmdImage)
					jsonWrite(w, 400, map[string]any{
						"error":  "image mismatch",
						"detail": fmt.Sprintf("The Docker image in your startup command (%s) does not match the template image (%s). Please use the correct image or leave the startup command empty to use the default.", cmdImage, req.Docker.Image),
					})
					return
				}
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
			if req.Docker.Command != "" {
				runtimeConfig["command"] = req.Docker.Command
			}
			if len(req.Docker.Ports) > 0 {
				runtimeConfig["ports"] = req.Docker.Ports
			}
			if len(req.Docker.Volumes) > 0 {
				runtimeConfig["volumes"] = req.Docker.Volumes
			}
			if len(req.Docker.Env) > 0 {
				runtimeConfig["env"] = req.Docker.Env
			}
			runtimeConfig["providerId"] = "custom"
		}

		if startupCmd != "" {
			runtimeConfig["startupCommand"] = startupCmd
			fmt.Printf("[createServer] Storing startupCommand in runtimeConfig: %q\n", startupCmd)
		}

		meta = map[string]any{
			"type":      template,
			"template":  template,
			"hostPort":  hostPort,
			"createdAt": time.Now().UnixMilli(),
		}
		if startupCmd != "" {
			meta["startupCommand"] = startupCmd
			fmt.Printf("[createServer] Storing startupCommand at top level: %q\n", startupCmd)
		}
		if len(runtimeConfig) > 0 {
			meta["runtime"] = runtimeConfig
		}
		if len(resourcesMeta) > 0 {
			meta["resources"] = resourcesMeta
		}

		fmt.Printf("[createServer] Final meta to save: %+v\n", meta)
		_ = a.saveMeta(serverDir, meta)

		if req.Docker != nil && req.Docker.Image != "" {
			imageRef := req.Docker.Image
			if req.Docker.Tag != "" {
				imageRef += ":" + req.Docker.Tag
			} else {
				imageRef += ":latest"
			}

			fmt.Printf("[createServer] Pulling Docker image: %s \n", imageRef)
			pullCtx, pullCancel := context.WithTimeout(context.Background(), 10*time.Minute)
			a.docker.pull(pullCtx, imageRef)
			pullCancel()
			fmt.Printf("[createServer] Docker image pulled successfully: %s\n", imageRef)

			ensureVolumeWritable(serverDir)

		}

		if req.ImportUrl != "" {
			fmt.Printf("[createServer] Importing archive from URL: %s\n", req.ImportUrl)
			if err := a.downloadAndExtractArchiveSync(name, serverDir, req.ImportUrl); err != nil {
				fmt.Printf("[createServer] Warning: archive import failed: %v\n", err)
			}
		}

		hasImage := req.Docker != nil && req.Docker.Image != ""
		hasStartupCmd := startupCmd != ""
		if !hasStartupCmd && hasImage {
			effectiveCmd := a.buildEffectiveDockerCommand(name, serverDir, meta)
			if effectiveCmd != "" {
				meta["startupCommand"] = effectiveCmd
				runtimeObj, _ := meta["runtime"].(map[string]any)
				if runtimeObj == nil {
					runtimeObj = make(map[string]any)
				}
				runtimeObj["startupCommand"] = effectiveCmd
				meta["runtime"] = runtimeObj
				_ = a.saveMeta(serverDir, meta)
				fmt.Printf("[createServer] Generated effective startupCommand: %s\n", effectiveCmd)
			}
		}

		if req.AutoStart && (hasImage || hasStartupCmd) {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			err := a.startCustomDockerContainer(ctx, name, serverDir, meta, hostPort, req.Resources)
			cancel()
			if err != nil {
				fmt.Printf("[createServer] Container startup failed, cleaning up: %s - %v\n", name, err)
				_ = os.RemoveAll(serverDir)
				jsonWrite(w, 500, map[string]any{"error": "failed to start server", "detail": err.Error()})
				return
			}
		}
	}

	auditLog.Log("server_create", getClientIP(r), "", name, "create", "success", map[string]any{"template": req.Template})
	jsonWrite(w, 200, map[string]any{"ok": true, "name": name, "meta": meta})
}

func (a *Agent) handleServerInfo(w http.ResponseWriter, r *http.Request, name string) {
	serverDir := a.serverDir(name)
	if st, err := os.Stat(serverDir); err != nil || !st.IsDir() {
		jsonWrite(w, 404, map[string]any{"error": "not found"})
		return
	}
	meta := a.loadMeta(serverDir)

	status := "stopped"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var cpuPercent float64 = 0
	var memoryUsedMb float64 = 0
	var memoryLimitMb float64 = 0
	var memoryPercent float64 = 0
	var uptimeSeconds int64 = 0

	if resources, ok := meta["resources"].(map[string]any); ok {
		if ramMb, ok := resources["ramMb"]; ok {
			switch v := ramMb.(type) {
			case float64:
				memoryLimitMb = v
			case int:
				memoryLimitMb = float64(v)
			case int64:
				memoryLimitMb = float64(v)
			}
		}
	}

	if a.docker.containerExists(ctx, name) {
		out, _, _, err := a.docker.runCollect(ctx, "inspect", "-f", "{{.State.Running}}", name)
		if err == nil && strings.TrimSpace(out) == "true" {
			status = "running"

			startedAt, _, _, startErr := a.docker.runCollect(ctx, "inspect", "-f", "{{.State.StartedAt}}", name)
			if startErr == nil && strings.TrimSpace(startedAt) != "" {
				startTime, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(startedAt))
				if parseErr == nil {
					uptimeSeconds = int64(time.Since(startTime).Seconds())
				}
			}

			statsOut, _, _, statsErr := a.docker.runCollect(ctx, "stats", "--no-stream", "--format",
				"{{.CPUPerc}}|{{.MemUsage}}|{{.MemPerc}}", name)
			if statsErr == nil && strings.TrimSpace(statsOut) != "" {
				parts := strings.Split(strings.TrimSpace(statsOut), "|")
				if len(parts) >= 3 {
					cpuStr := strings.TrimSuffix(strings.TrimSpace(parts[0]), "%")
					if cpu, err := strconv.ParseFloat(cpuStr, 64); err == nil {
						cpuPercent = cpu
					}

					memParts := strings.Split(parts[1], "/")
					if len(memParts) >= 2 {
						memoryUsedMb = parseMemoryString(strings.TrimSpace(memParts[0]))
						dockerMemLimit := parseMemoryString(strings.TrimSpace(memParts[1]))
						if dockerMemLimit > 0 {
							memoryLimitMb = dockerMemLimit
						}
					}

					memPctStr := strings.TrimSuffix(strings.TrimSpace(parts[2]), "%")
					if memPct, err := strconv.ParseFloat(memPctStr, 64); err == nil {
						memoryPercent = memPct
					}
				}
			}
		} else if err != nil {
			status = "unknown"
		}
	}

	diskUsageBytes := a.getServerDiskUsage(serverDir)
	diskUsageMb := float64(diskUsageBytes) / (1024 * 1024)
	diskUsageGb := float64(diskUsageBytes) / (1024 * 1024 * 1024)

	var storageLimitMb float64 = 0
	var storageLimitGb int64 = 0
	if resources, ok := meta["resources"].(map[string]any); ok {
		if limit, ok := resources["storageMb"]; ok {
			var limitMb int64
			switch v := limit.(type) {
			case float64:
				limitMb = int64(v)
			case int:
				limitMb = int64(v)
			case int64:
				limitMb = v
			}
			storageLimitMb = float64(limitMb)
			storageLimitGb = int64(storageLimitMb / 1024)
		} else if limit, ok := resources["storageGb"]; ok {
			switch v := limit.(type) {
			case float64:
				storageLimitGb = int64(v)
			case int:
				storageLimitGb = int64(v)
			case int64:
				storageLimitGb = v
			}
			storageLimitMb = float64(storageLimitGb) * 1024
		}
	}

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
		diskPercent = (float64(diskUsageBytes) / float64(limitBytes)) * 100
		diskInfo["percentUsed"] = diskPercent
	}

	cpuCoresConfig := metaCpuCores(meta)
	cpuEffCores, cpuMaxPercent := computeCpuLimit(cpuCoresConfig)
	if cpuPercent > cpuMaxPercent {
		cpuPercent = cpuMaxPercent
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

	jsonWrite(w, 200, map[string]any{
		"ok":     true,
		"name":   name,
		"status": status,
		"meta":   meta,
		"disk":   diskInfo,
		"stats":  stats,
		"uptime": uptimeSeconds,
	})
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
	startupCommand := ""
	if sc, ok := meta["startupCommand"].(string); ok && strings.TrimSpace(sc) != "" {
		startupCommand = sc
	} else if runtimeObj, ok := meta["runtime"].(map[string]any); ok {
		if sc, ok := runtimeObj["startupCommand"].(string); ok && strings.TrimSpace(sc) != "" {
			startupCommand = sc
		}
	}

	if startupCommand == "" {
		startupCommand = a.buildEffectiveDockerCommand(name, serverDir, meta)
	}

	template := ""
	if t, ok := meta["template"].(string); ok {
		template = t
	} else if t, ok := meta["type"].(string); ok {
		template = t
	}

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
			statsOut, _, _, statsErr := a.docker.runCollect(ctx, "stats", "--no-stream", "--format",
				"{{.MemUsage}}", name)
			if statsErr == nil && strings.TrimSpace(statsOut) != "" {
				memParts := strings.Split(strings.TrimSpace(statsOut), "/")
				if len(memParts) >= 2 {
					memoryUsedMb = parseMemoryString(strings.TrimSpace(memParts[0]))
					dockerMemLimit := parseMemoryString(strings.TrimSpace(memParts[1]))
					if dockerMemLimit > 0 {
						memoryLimitMb = dockerMemLimit
					}
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
	if hp, ok := meta["hostPort"]; ok {
		switch v := hp.(type) {
		case float64:
			hostPort = int(v)
		case int:
			hostPort = v
		case int64:
			hostPort = int(v)
		}
	}

	jsonWrite(w, 200, map[string]any{
		"ok":             true,
		"resources":      resources,
		"startupCommand": startupCommand,
		"template":       template,
		"stats":          stats,
		"hostPort":       hostPort,
	})
}

func (a *Agent) buildEffectiveDockerCommand(name, serverDir string, meta map[string]any) string {
	typ := strings.ToLower(fmt.Sprint(meta["type"]))
	runtimeObj, _ := meta["runtime"].(map[string]any)
	if runtimeObj == nil {
		runtimeObj = make(map[string]any)
	}

	hostPort := anyToInt(meta["hostPort"])

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
		parts := []string{"docker run -d -t",
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

		parts := []string{"docker run -d -t", "--name", name, "--restart", "unless-stopped"}
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
		if hostPort > 0 {
			parts = append(parts, "-p", fmt.Sprintf("%d:%d", hostPort, hostPort))
		}
		portForwardsRT := a.loadPortForwards(serverDir)
		for _, pf := range portForwardsRT {
			parts = append(parts, "-p", fmt.Sprintf("%d:%d", pf.Public, pf.Internal))
		}
		vols := []string{fmt.Sprintf("%s:/app", serverDir)}
		if vv, ok := runtimeObj["volumes"].([]any); ok && len(vv) > 0 {
			vols = vols[:0]
			for _, v := range vv {
				s := strings.TrimSpace(fmt.Sprint(v))
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
		if ev, ok := runtimeObj["env"].(map[string]any); ok {
			for k, v := range ev {
				parts = append(parts, "-e", fmt.Sprintf("%s=%s", k, fmt.Sprint(v)))
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
				cmd = fmt.Sprintf("cd /app && npm install && node /app/%s", startFile)
			}
		}
		if cmd != "" {
			parts = append(parts, "sh", "-lc", fmt.Sprintf("%q", cmd))
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
		parts := []string{"docker run -d -t", "--name", name, "--restart", "unless-stopped"}
		if hasRam {
			parts = append(parts, "--memory", fmt.Sprintf("%dm", int(ramMb)))
			parts = append(parts, "--memory-reservation", fmt.Sprintf("%dm", int(ramMb*90/100)))
		}
		if hasCpu && cpuCoresVal <= 128 {
			cpuQuota := int64(float64(cpuPeriod) * cpuCoresVal)
			parts = append(parts, "--cpu-period", fmt.Sprintf("%d", cpuPeriod))
			parts = append(parts, "--cpu-quota", fmt.Sprintf("%d", cpuQuota))
		}
		defaultContainerPort := hostPort
		if ports, ok := runtimeObj["ports"].([]any); ok && len(ports) > 0 {
			switch v := ports[0].(type) {
			case float64:
				if int(v) > 0 {
					defaultContainerPort = int(v)
				}
			case int:
				if v > 0 {
					defaultContainerPort = v
				}
			case string:
				if p, _ := strconv.Atoi(v); p > 0 {
					defaultContainerPort = p
				}
			}
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
		if vv, ok := runtimeObj["volumes"].([]any); ok && len(vv) > 0 {
			for _, v := range vv {
				s := strings.TrimSpace(fmt.Sprint(v))
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
		if ev, ok := runtimeObj["env"].(map[string]any); ok {
			for k, v := range ev {
				parts = append(parts, "-e", fmt.Sprintf("%s=%s", k, fmt.Sprint(v)))
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
			RamMb          *int     `json:"ramMb"`
			CpuCores       *float64 `json:"cpuCores"`
			StorageMb      *int     `json:"storageMb"`
			StorageGb      *int     `json:"storageGb"`
			SwapMb         *int     `json:"swapMb"`
			BackupsMax     *int     `json:"backupsMax"`
			MaxSchedules   *int     `json:"maxSchedules"`
			Ports          []int    `json:"ports"`
			StartupCommand *string  `json:"startupCommand"`
		} `json:"resources"`
		StartupCommand *string `json:"startupCommand"`
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
	if req.Resources.Ports != nil {
		validPorts := make([]int, 0, len(req.Resources.Ports))
		for _, p := range req.Resources.Ports {
			if p >= 1 && p <= 65535 {
				validPorts = append(validPorts, p)
			}
		}
		resources["ports"] = validPorts
	}

	meta["resources"] = resources

	startupCmd := ""
	if req.StartupCommand != nil {
		startupCmd = strings.TrimSpace(*req.StartupCommand)
	} else if req.Resources.StartupCommand != nil {
		startupCmd = strings.TrimSpace(*req.Resources.StartupCommand)
	}
	if startupCmd != "" {
		allocatedPort := anyToInt(meta["hostPort"])
		if allocatedPort > 0 {
			var reservedPorts []int
			if pfList, ok := meta["portForwards"].([]any); ok {
				for _, item := range pfList {
					if m, ok := item.(map[string]any); ok {
						if pub := anyToInt(m["publicPort"]); pub > 0 {
							reservedPorts = append(reservedPorts, pub)
						}
					}
				}
			}
			var additionalPorts []int
			if metaRes, ok := meta["resources"].(map[string]any); ok {
				if ports, ok := metaRes["ports"].([]any); ok {
					for _, p := range ports {
						if v := anyToInt(p); v > 0 {
							additionalPorts = append(additionalPorts, v)
						}
					}
				}
			}
			if req.Resources.Ports != nil {
				for _, p := range req.Resources.Ports {
					if p > 0 {
						additionalPorts = append(additionalPorts, p)
					}
				}
			}
			portViolation := validateStartupCommandPorts(startupCmd, allocatedPort, additionalPorts, reservedPorts...)
			if portViolation != "" {
				fmt.Printf("[port-enforce] BLOCKED port change for %s: %s\n", name, portViolation)
				jsonWrite(w, 400, map[string]any{"error": portViolation})
				return
			}
		}

		newImage := extractDockerImageFromCommand(startupCmd)
		if newImage == "" && strings.HasPrefix(strings.ToLower(strings.TrimSpace(startupCmd)), "docker run") {
			jsonWrite(w, 400, map[string]any{"error": "could not determine Docker image from startup command"})
			return
		}

		runtimeObj, _ := meta["runtime"].(map[string]any)
		if runtimeObj == nil {
			runtimeObj = make(map[string]any)
		}

		currentImage := ""
		if img, ok := runtimeObj["image"].(string); ok && strings.TrimSpace(img) != "" {
			tag := "latest"
			if t, ok := runtimeObj["tag"].(string); ok && strings.TrimSpace(t) != "" {
				tag = strings.TrimSpace(t)
			}
			currentImage = strings.TrimSpace(img) + ":" + tag
		}
		if currentImage == "" {
			existingCmd := ""
			if cmd, ok := runtimeObj["startupCommand"].(string); ok {
				existingCmd = cmd
			} else if cmd, ok := meta["startupCommand"].(string); ok {
				existingCmd = cmd
			}
			if existingCmd != "" {
				currentImage = extractDockerImageFromCommand(existingCmd)
			}
		}

		if currentImage != "" && newImage != "" {
			normalizedCurrent := normalizeDockerImage(currentImage)
			normalizedNew := normalizeDockerImage(newImage)
			if normalizedCurrent != normalizedNew {
				fmt.Printf("[security] BLOCKED docker image change for %s: %q -> %q\n", name, currentImage, newImage)
				jsonWrite(w, 403, map[string]any{"error": "changing the Docker image is not allowed; you may only modify command flags and arguments"})
				return
			}
		}

		runtimeObj["startupCommand"] = startupCmd
		meta["runtime"] = runtimeObj
		meta["startupCommand"] = startupCmd
		fmt.Printf("[resources] Updated startupCommand for %s: %s\n", name, startupCmd)
	}

	if err := a.saveMeta(serverDir, meta); err != nil {
		jsonWrite(w, 500, map[string]any{"error": "failed to save meta"})
		return
	}

	if a.debug {
		a.logf("[resources] Updated resources for %s: %+v", name, resources)
	}

	jsonWrite(w, 200, map[string]any{"ok": true, "resources": resources})
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
				pub := anyToInt(m["publicPort"])
				internal := anyToInt(m["internalPort"])
				if pub > 0 && internal > 0 {
					portForwards = append(portForwards, map[string]any{
						"publicPort":   pub,
						"internalPort": internal,
					})
				}
			}
		}
	}

	hostPort := anyToInt(meta["hostPort"])
	allocatedPorts := []int{}
	if hostPort > 0 {
		allocatedPorts = append(allocatedPorts, hostPort)
	}
	if resources, ok := meta["resources"].(map[string]any); ok {
		if ports, ok := resources["ports"].([]any); ok {
			for _, p := range ports {
				v := anyToInt(p)
				if v > 0 {
					allocatedPorts = append(allocatedPorts, v)
				}
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
	hostPort := anyToInt(meta["hostPort"])
	if hostPort > 0 {
		allocatedSet[hostPort] = true
	}
	if resources, ok := meta["resources"].(map[string]any); ok {
		if ports, ok := resources["ports"].([]any); ok {
			for _, p := range ports {
				v := anyToInt(p)
				if v > 0 {
					allocatedSet[v] = true
				}
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
		pub := anyToInt(m["publicPort"])
		internal := anyToInt(m["internalPort"])
		if pub > 0 && internal > 0 {
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
	allowedExts := []string{".zip", ".tar.gz", ".tgz", ".tar.bz2", ".tbz2", ".7z", ".rar"}
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
	allowedExts := []string{".zip", ".tar.gz", ".tgz", ".tar.bz2", ".tbz2", ".7z", ".rar"}
	for _, ext := range allowedExts {
		if strings.HasSuffix(pathname, ext) {
			validExt = true
			break
		}
	}
	if !validExt {
		return fmt.Errorf("unsupported archive format (supported: .zip, .tar.gz, .tgz, .tar.bz2, .tbz2, .7z, .rar)")
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
	URL      string `json:"url"`
	NodeID   string `json:"nodeId"`
	DestPath string `json:"destPath"`
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

		var resources *resourceLimits
		if rm, ok := meta["resources"].(map[string]any); ok {
			resources = &resourceLimits{}
			if v, ok := rm["ramMb"].(float64); ok {
				i := int(v)
				resources.RamMb = &i
			}
			if v, ok := rm["cpuCores"].(float64); ok {
				resources.CpuCores = &v
			}
			if v, ok := rm["storageMb"].(float64); ok {
				i := int(v)
				resources.StorageMb = &i
			} else if v, ok := rm["storageGb"].(float64); ok {
				i := int(v) * 1024
				resources.StorageMb = &i
			}
			if v, ok := rm["swapMb"].(float64); ok {
				i := int(v)
				resources.SwapMb = &i
			}
			if v, ok := rm["backupsMax"].(float64); ok {
				i := int(v)
				resources.BackupsMax = &i
			}
		}

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
	if req.NodeID != "" && a.uuid != "" && req.NodeID != a.uuid {
		jsonWrite(w, 401, map[string]any{"ok": false, "error": "unauthorized"})
		return
	}

	serverDir := a.serverDir(name)

	existingMeta := a.loadMeta(serverDir)

	hasCustomConfig := false
	if existingMeta != nil {
		if _, ok := existingMeta["startupCommand"]; ok {
			hasCustomConfig = true
		}
		if rt, ok := existingMeta["runtime"].(map[string]any); ok {
			if _, ok := rt["startupCommand"]; ok {
				hasCustomConfig = true
			}
		}
	}

	if !hasCustomConfig {
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
		hostPort = clampPort(req.Port, 3001)
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
		"runtime": req.Runtime,
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
		if cmd, ok := existingMeta["startupCommand"]; ok {
			meta["startupCommand"] = cmd
		}
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
			if cmd, ok := existingRt["startupCommand"]; ok {
				newRt["startupCommand"] = cmd
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
	a.docker.pull(ctx, ref)
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
	typ := strings.ToLower(fmt.Sprint(meta["type"]))
	if a.debug {
		fmt.Printf("[handleStart] Server: %s, type: %s\n", name, typ)
	}

	var body struct {
		HostPort int               `json:"hostPort"`
		Env      map[string]string `json:"env"`
	}
	_ = readJSON(w, r, a.uploadLimit, &body)

	var resources *resourceLimits
	if rm, ok := meta["resources"].(map[string]any); ok {
		resources = &resourceLimits{}
		if v, ok := rm["ramMb"].(float64); ok {
			i := int(v)
			resources.RamMb = &i
		}
		if v, ok := rm["cpuCores"].(float64); ok {
			resources.CpuCores = &v
		}
		if v, ok := rm["storageMb"].(float64); ok {
			i := int(v)
			resources.StorageMb = &i
		} else if v, ok := rm["storageGb"].(float64); ok {
			i := int(v) * 1024
			resources.StorageMb = &i
		}
		if v, ok := rm["swapMb"].(float64); ok {
			i := int(v)
			resources.SwapMb = &i
		}
		if v, ok := rm["backupsMax"].(float64); ok {
			i := int(v)
			resources.BackupsMax = &i
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	userStartupCmd := ""
	if sc, ok := meta["startupCommand"].(string); ok && strings.TrimSpace(sc) != "" {
		userStartupCmd = sc
	} else if runtimeObj, ok := meta["runtime"].(map[string]any); ok {
		if sc, ok := runtimeObj["startupCommand"].(string); ok && strings.TrimSpace(sc) != "" {
			userStartupCmd = sc
		}
	}
	if userStartupCmd != "" {
		hp := body.HostPort
		if hp == 0 {
			hp = anyToInt(meta["hostPort"])
		}
		if hp == 0 {
			if typ == "minecraft" {
				hp = 25565
			} else {
				hp = 8080
			}
		}
		if err := a.executeStartupCommand(ctx, name, serverDir, hp, userStartupCmd, resources, meta); err != nil {
			jsonWrite(w, 500, map[string]any{"error": "failed to start container", "detail": err.Error()})
			return
		}
		auditLog.Log("server_start", getClientIP(r), "", name, "start", "success", map[string]any{"type": typ, "customCommand": true})
		a.broadcastContainerEvent(name, "running")
		jsonWrite(w, 200, map[string]any{"ok": true})
		return
	}

	if typ == "minecraft" {
		hp := body.HostPort
		if hp == 0 {
			hp = anyToInt(meta["hostPort"])
		}
		if hp == 0 {
			hp = 25565
		}
		enforceServerProps(serverDir)
		if err := a.startMinecraftContainer(ctx, name, serverDir, hp, resources, body.Env); err != nil {
			jsonWrite(w, 500, map[string]any{"error": "failed to start container"})
			return
		}
		auditLog.Log("server_start", getClientIP(r), "", name, "start", "success", map[string]any{"type": "minecraft"})
		a.broadcastContainerEvent(name, "running")
		jsonWrite(w, 200, map[string]any{"ok": true})
		return
	}

	if meta["runtime"] != nil && (typ == "python" || typ == "nodejs" || typ == "discord-bot" || typ == "runtime") {
		hp := body.HostPort
		if hp == 0 {
			hp = anyToInt(meta["hostPort"])
		}
		if err := a.startRuntimeContainer(ctx, name, serverDir, meta, hp, resources, body.Env); err != nil {
			jsonWrite(w, 500, map[string]any{"error": "failed to start container"})
			return
		}
		auditLog.Log("server_start", getClientIP(r), "", name, "start", "success", map[string]any{"type": typ})
		a.broadcastContainerEvent(name, "running")
		jsonWrite(w, 200, map[string]any{"ok": true})
		return
	}

	if startupCmd, ok := meta["startupCommand"].(string); ok && strings.TrimSpace(startupCmd) != "" {
		runtimeObj, _ := meta["runtime"].(map[string]any)
		if runtimeObj == nil {
			runtimeObj = make(map[string]any)
		}
		runtimeObj["startupCommand"] = startupCmd
		meta["runtime"] = runtimeObj
	}

	if typ != "minecraft" && typ != "python" && typ != "nodejs" && typ != "discord-bot" && typ != "runtime" {
		hp := body.HostPort
		if hp == 0 {
			hp = anyToInt(meta["hostPort"])
		}
		if hp == 0 {
			hp = 8080
		}

		if meta["runtime"] == nil {
			meta["runtime"] = make(map[string]any)
		}

		if err := a.startCustomDockerContainer(ctx, name, serverDir, meta, hp, resources); err != nil {
			jsonWrite(w, 500, map[string]any{"error": "failed to start container", "detail": err.Error()})
			return
		}
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
		_ = a.docker.sendCommand(ctx, a.stdinMgr, name, "stop", true)
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
	_, errStr, _, err := a.docker.runCollect(ctx, "restart", name)
	if err != nil {
		if a.debug {
			fmt.Printf("[docker] restart error for %s: %s\n", name, strings.TrimSpace(errStr))
		}
		jsonWrite(w, 500, map[string]any{"error": "container restart failed"})
		return
	}
	a.broadcastContainerEvent(name, "running")
	jsonWrite(w, 200, map[string]any{"ok": true})
}

func (a *Agent) handleCommand(w http.ResponseWriter, r *http.Request, name string) {
	if !isValidContainerName(name) {
		jsonWrite(w, 400, map[string]any{"error": "invalid container name"})
		return
	}

	var body struct {
		Command string `json:"command"`
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

	serverDir := a.serverDir(name)
	if st, err := os.Stat(serverDir); err != nil || !st.IsDir() {
		jsonWrite(w, 404, map[string]any{"error": "not found"})
		return
	}

	meta := a.loadMeta(serverDir)
	serverType := strings.ToLower(fmt.Sprint(meta["type"]))
	isMinecraft := serverType == "" || serverType == "minecraft"

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	if err := a.docker.sendCommand(ctx, a.stdinMgr, name, cmd, isMinecraft); err != nil {
		jsonWrite(w, 200, map[string]any{
			"ok":     false,
			"error":  "failed-to-send",
			"detail": err.Error(),
			"type":   serverType,
		})
		return
	}

	jsonWrite(w, 200, map[string]any{"ok": true, "type": serverType})
}

func (a *Agent) handleServerCommandBody(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string `json:"name"`
		Command string `json:"command"`
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
	sendLine := func(line string) {
		payload := map[string]any{"line": cleanLogLine(line)}
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", string(b))
		flusher.Flush()
	}

	sendEvent("hello", map[string]any{"server": name, "ts": time.Now().UnixMilli(), "tail": tailN})

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	out, _, _, _ := a.docker.runCollect(ctx, "logs", "--tail", fmt.Sprint(tailN), name)
	cancel()
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line != "" {
			sendLine(line)
		}
	}

	t := a.logs.getOrCreate(name)
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
		case <-hb.C:
			sendEvent("keepalive", map[string]any{"ts": time.Now().UnixMilli()})
		case s := <-ch:
			line := s
			if strings.HasPrefix(line, "stdout:") {
				line = strings.TrimPrefix(line, "stdout:")
			} else if strings.HasPrefix(line, "stderr:") {
				line = strings.TrimPrefix(line, "stderr:")
			}
			sendLine(line)
		}
	}
}

func (a *Agent) handleKill(w http.ResponseWriter, r *http.Request, name string) {
	a.logs.killTail(name)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.docker.updateRestartNo(ctx, name)
	a.docker.rmForce(ctx, name)
	a.broadcastContainerEvent(name, "stopped")
	jsonWrite(w, 200, map[string]any{"ok": true})
}

func (a *Agent) handleDeleteServer(w http.ResponseWriter, r *http.Request, name string) {
	if name == "" {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid name"})
		return
	}
	a.logs.killTail(name)

	filesOnly := r.URL.Query().Get("files_only") == "true"
	if !filesOnly {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		a.docker.updateRestartNo(ctx, name)
		a.docker.rmForce(ctx, name)
		cancel()
	}

	serverDir := a.serverDir(name)
	backupsDir := a.serverBackupsDir(name)
	fmt.Printf("[delete] attempting to remove server directory: %s\n", serverDir)

	if filepath.Clean(serverDir) == filepath.Clean(a.volumesDir) {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "refusing to delete volumes root"})
		return
	}
	if !isUnder(a.volumesDir, serverDir) {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid path"})
		return
	}

	if _, err := os.Stat(serverDir); os.IsNotExist(err) {
		fmt.Printf("[delete] directory already gone: %s\n", serverDir)
		_ = os.Remove(a.metaPath(name))
		if isUnder(a.backupsDir(), backupsDir) {
			_ = os.RemoveAll(backupsDir)
		}
		auditLog.Log("server_delete", getClientIP(r), "", name, "delete", "success", nil)
		jsonWrite(w, 200, map[string]any{"ok": true, "name": name})
		return
	}

	if err := os.RemoveAll(serverDir); err != nil {
		fmt.Printf("[delete] error removing %s: %v\n", serverDir, err)
		jsonWrite(w, 500, map[string]any{"ok": false, "error": "failed to delete server"})
		return
	}
	_ = os.Remove(a.metaPath(name))
	if isUnder(a.backupsDir(), backupsDir) {
		_ = os.RemoveAll(backupsDir)
	}

	fmt.Printf("[delete] successfully removed: %s\n", serverDir)
	auditLog.Log("server_delete", getClientIP(r), "", name, "delete", "success", nil)
	jsonWrite(w, 200, map[string]any{"ok": true, "name": name})
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
	}
	if err := readJSON(w, r, a.uploadLimit, &req); err != nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "bad json", "detail": err.Error()})
		return
	}

	template := strings.ToLower(strings.TrimSpace(req.TemplateId))
	if template == "" {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "missing templateId"})
		return
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
	existingResources, _ := oldMeta["resources"].(map[string]any)

	existingStartupCmd := ""
	if sc, ok := oldMeta["startupCommand"].(string); ok && strings.TrimSpace(sc) != "" {
		existingStartupCmd = sc
	} else if rt, ok := oldMeta["runtime"].(map[string]any); ok {
		if sc, ok := rt["startupCommand"].(string); ok && strings.TrimSpace(sc) != "" {
			existingStartupCmd = sc
		}
	}
	if existingStartupCmd != "" {
		fmt.Printf("[reinstall] Preserving active startupCommand from old meta for %s\n", name)
	}

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
		if req.Docker != nil && len(req.Docker.Ports) > 0 {
			hostPort = req.Docker.Ports[0]
		} else {
			hostPort = 8080
		}
	}

	startupCmd := ""
	if existingStartupCmd != "" {
		startupCmd = existingStartupCmd
		fmt.Printf("[reinstall] Using preserved active startupCommand for %s\n", name)
	} else if req.StartupCommand != "" {
		startupCmd = req.StartupCommand
	} else if req.Docker != nil && req.Docker.StartupCommand != "" {
		startupCmd = req.Docker.StartupCommand
	}

	runtimeConfig := map[string]any{}
	if req.Docker != nil {
		if req.Docker.Image != "" {
			runtimeConfig["image"] = req.Docker.Image
		}
		if req.Docker.Tag != "" {
			runtimeConfig["tag"] = req.Docker.Tag
		}
		if req.Docker.Command != "" {
			runtimeConfig["command"] = req.Docker.Command
		}
		if len(req.Docker.Ports) > 0 {
			runtimeConfig["ports"] = req.Docker.Ports
		}
		if len(req.Docker.Volumes) > 0 {
			runtimeConfig["volumes"] = req.Docker.Volumes
		}
		if len(req.Docker.Env) > 0 {
			runtimeConfig["env"] = req.Docker.Env
		}
		runtimeConfig["providerId"] = "custom"
	}
	if startupCmd != "" {
		runtimeConfig["startupCommand"] = startupCmd
	}

	meta := map[string]any{
		"type":      template,
		"template":  template,
		"hostPort":  hostPort,
		"createdAt": time.Now().UnixMilli(),
	}
	if startupCmd != "" {
		meta["startupCommand"] = startupCmd
	}
	if len(runtimeConfig) > 0 {
		meta["runtime"] = runtimeConfig
	}
	if existingResources != nil && len(existingResources) > 0 {
		meta["resources"] = existingResources
	}
	if existingPF, ok := oldMeta["portForwards"]; ok {
		meta["portForwards"] = existingPF
	}

	if template == "minecraft" {
		if hostPort == 0 || hostPort == 8080 {
			hostPort = 25565
			meta["hostPort"] = hostPort
		}
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

		meta = map[string]any{
			"type":     "minecraft",
			"fork":     fork,
			"version":  ver,
			"start":    "server.jar",
			"hostPort": hostPort,
		}
		if existingResources != nil && len(existingResources) > 0 {
			meta["resources"] = existingResources
		}
		if existingPF, ok := oldMeta["portForwards"]; ok {
			meta["portForwards"] = existingPF
		}
		_ = a.saveMeta(serverDir, meta)
		enforceServerProps(serverDir)

		if existingStartupCmd != "" {
			meta["startupCommand"] = existingStartupCmd
			runtimeObj, _ := meta["runtime"].(map[string]any)
			if runtimeObj == nil {
				runtimeObj = make(map[string]any)
			}
			runtimeObj["startupCommand"] = existingStartupCmd
			meta["runtime"] = runtimeObj
			_ = a.saveMeta(serverDir, meta)
			fmt.Printf("[reinstall] Preserved active startupCommand for minecraft server %s\n", name)
		} else {
			effectiveCmd := a.buildEffectiveDockerCommand(name, serverDir, meta)
			if effectiveCmd != "" {
				meta["startupCommand"] = effectiveCmd
				runtimeObj, _ := meta["runtime"].(map[string]any)
				if runtimeObj == nil {
					runtimeObj = make(map[string]any)
				}
				runtimeObj["startupCommand"] = effectiveCmd
				meta["runtime"] = runtimeObj
				_ = a.saveMeta(serverDir, meta)
			}
		}
	} else {
		_ = a.saveMeta(serverDir, meta)

		imagePulled := false
		if req.Docker != nil && req.Docker.Image != "" {
			imageRef := req.Docker.Image
			if req.Docker.Tag != "" {
				imageRef += ":" + req.Docker.Tag
			} else {
				imageRef += ":latest"
			}

			fmt.Printf("[reinstall] Pulling Docker image: %s\n", imageRef)
			pullCtx, pullCancel := context.WithTimeout(context.Background(), 10*time.Minute)
			a.docker.pull(pullCtx, imageRef)
			pullCancel()
			fmt.Printf("[reinstall] Docker image pulled: %s\n", imageRef)
			imagePulled = true

			ensureVolumeWritable(serverDir)
		}

		if !imagePulled {
			if img, ok := runtimeConfig["image"].(string); ok && strings.TrimSpace(img) != "" {
				tag := "latest"
				if t, ok := runtimeConfig["tag"].(string); ok && strings.TrimSpace(t) != "" {
					tag = t
				}
				fallbackRef := strings.TrimSpace(img) + ":" + tag
				fmt.Printf("[reinstall] Pre-pulling Docker image from runtime config: %s\n", fallbackRef)
				pullCtx, pullCancel := context.WithTimeout(context.Background(), 10*time.Minute)
				a.docker.pull(pullCtx, fallbackRef)
				pullCancel()
				fmt.Printf("[reinstall] Docker image pre-pulled: %s\n", fallbackRef)
				ensureVolumeWritable(serverDir)
				imagePulled = true
			}
		}

		if !imagePulled && startupCmd != "" {
			cmdLower := strings.ToLower(strings.TrimSpace(startupCmd))
			if strings.HasPrefix(cmdLower, "docker run") {
				tempArgs := parseShellArgs(strings.TrimSpace(startupCmd[len("docker run"):]))
				skipN := false
				for _, tArg := range tempArgs {
					if skipN {
						skipN = false
						continue
					}
					if strings.HasPrefix(tArg, "-") {
						if strings.Contains(tArg, "=") {
							continue
						}
						skipN = true
						continue
					}
					candidate := strings.TrimSpace(tArg)
					if candidate != "" && !strings.HasPrefix(candidate, "{") {
						fmt.Printf("[reinstall] Pre-pulling Docker image from startupCommand: %s\n", candidate)
						pullCtx, pullCancel := context.WithTimeout(context.Background(), 10*time.Minute)
						a.docker.pull(pullCtx, candidate)
						pullCancel()
						fmt.Printf("[reinstall] Docker image pre-pulled: %s\n", candidate)
						ensureVolumeWritable(serverDir)
					}
					break
				}
			}
		}

		if startupCmd == "" && req.Docker != nil && req.Docker.Image != "" {
			effectiveCmd := a.buildEffectiveDockerCommand(name, serverDir, meta)
			if effectiveCmd != "" {
				meta["startupCommand"] = effectiveCmd
				runtimeObj, _ := meta["runtime"].(map[string]any)
				if runtimeObj == nil {
					runtimeObj = make(map[string]any)
				}
				runtimeObj["startupCommand"] = effectiveCmd
				meta["runtime"] = runtimeObj
				_ = a.saveMeta(serverDir, meta)
				fmt.Printf("[reinstall] Generated effective startupCommand: %s\n", effectiveCmd)
			}
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
	ents, _ := os.ReadDir(dir)
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
	ents, _ := os.ReadDir(abs)
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

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "bad multipart"})
		return
	}
	defer r.MultipartForm.RemoveAll()

	dirStr := r.FormValue("dir")
	absDir, ok := a.safeUnderVolumes(dirStr)
	if !ok {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "invalid dir"})
		return
	}
	var uploadSize int64
	if r.ContentLength > 0 {
		uploadSize = r.ContentLength
	}
	if diskErr := a.checkDiskLimitForPath(absDir, uploadSize); diskErr != nil {
		diskErr["ok"] = false
		jsonWrite(w, 507, diskErr)
		return
	}
	if err := ensureResolvedUnder(a.volumesDir, absDir); err != nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "symlink escape blocked"})
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		jsonWrite(w, 400, map[string]any{"ok": false, "error": "missing file field"})
		return
	}
	defer file.Close()

	rawName := filepath.Base(header.Filename)
	safeName := sanitizeName(rawName)
	if safeName == "" {
		safeName = "upload.bin"
	}

	_ = ensureDir(absDir)
	destPath := filepath.Join(absDir, safeName)

	if info, err := os.Lstat(destPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			jsonWrite(w, 400, map[string]any{"ok": false, "error": "cannot overwrite symlink"})
			return
		}
	}

	out, err := os.OpenFile(destPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		if a.debug {
			fmt.Printf("[fs] upload stream create error: %v\n", err)
		}
		jsonWrite(w, 500, map[string]any{"ok": false, "error": "file create failed"})
		return
	}
	defer out.Close()

	if _, err := io.Copy(out, file); err != nil {
		if a.debug {
			fmt.Printf("[fs] upload stream write error: %v\n", err)
		}
		jsonWrite(w, 500, map[string]any{"ok": false, "error": "write failed"})
		return
	}

	jsonWrite(w, 200, map[string]any{"ok": true, "path": destPath})
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
		panelHMAC:   os.Getenv("PANEL_HMAC_SECRET"),
		stopGrace:   time.Duration(stopGraceMS) * time.Millisecond,
		docker:      Docker{debug: debug},
		dl: Downloader{
			headerTimeout: time.Duration(headerTimeoutMS) * time.Millisecond,
			maxBytes:      downloadMax,
		},
		stoppingNow:    make(map[string]time.Time),
		importProgress: make(map[string]*ImportProgress),
		importSem:      make(chan struct{}, 3),
	}
	agent.logs = newLogManager(agent.docker, debug)
	agent.stdinMgr = newStdinManager(&agent.docker, debug)

	protectNodeProcess()

	startRateLimitCleanup()

	diskMon := newDiskMonitor(agent, 60*time.Second, debug)
	diskMon.start()

	memMon := newMemoryMonitor(agent, 10*time.Second, debug)
	memMon.start()

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
