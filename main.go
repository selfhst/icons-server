package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	logLevelDebug = 0
	logLevelInfo  = 1
	logLevelError = 2
)

const (
	svgType  = "image/svg+xml"
	webpType = "image/webp"

	maxRemoteBody = 10 << 20

	maxMissTTL = 60 * time.Second

	svgCSP = "default-src 'none'; style-src 'unsafe-inline'; sandbox"
)

type Config struct {
	Port          string
	IconSource    string
	RemoteURL     string
	LocalPath     string
	PrimaryColor  string
	CacheTTL      time.Duration
	CacheSize     int
	RemoteTimeout time.Duration
	CORSOrigins   []string
	LogLevel      int
}

type entry struct {
	body        []byte
	gz          []byte
	contentType string
	etag        string
	stored      time.Time
}

func newEntry(body []byte, contentType, etag string) *entry {
	if etag == "" {
		etag = computeETag(body)
	}
	e := &entry{body: body, contentType: contentType, etag: etag}
	if contentType == svgType {
		e.gz = gzipBytes(body)
	}
	return e
}

type Cache struct {
	items map[string]*entry
	mutex sync.RWMutex
	ttl   time.Duration
	max   int
}

func NewCache(ttl time.Duration, maxSize int) *Cache {
	return &Cache{
		items: make(map[string]*entry),
		ttl:   ttl,
		max:   maxSize,
	}
}

func (c *Cache) Get(key string) (*entry, bool) {
	c.mutex.RLock()
	e, exists := c.items[key]
	c.mutex.RUnlock()

	if !exists {
		return nil, false
	}

	if time.Since(e.stored) > c.ttl {
		c.mutex.Lock()
		if c.items[key] == e {
			delete(c.items, key)
		}
		c.mutex.Unlock()
		return nil, false
	}

	return e, true
}

func (c *Cache) cleanup() {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	for k, v := range c.items {
		if time.Since(v.stored) > c.ttl {
			delete(c.items, k)
		}
	}
}

func (c *Cache) Set(key string, e *entry) {
	e.stored = time.Now()

	c.mutex.Lock()
	defer c.mutex.Unlock()

	if _, replacing := c.items[key]; !replacing && len(c.items) >= c.max {
		var oldestKey string
		var oldestTime time.Time
		first := true

		for k, v := range c.items {
			if first || v.stored.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.stored
				first = false
			}
		}
		delete(c.items, oldestKey)
	}

	c.items[key] = e
}

func formatDuration(d time.Duration) string {
	if d >= time.Second {
		return fmt.Sprintf("%.2fs", d.Seconds())
	}
	return fmt.Sprintf("%.2fms", float64(d)/float64(time.Millisecond))
}

func computeETag(content []byte) string {
	h := fnv.New64a()
	h.Write(content)
	return fmt.Sprintf(`"%x"`, h.Sum64())
}

func gzipBytes(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(b)
	zw.Close()
	return buf.Bytes()
}

func parseIntEnv(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n
	}
	log.Printf("[WARN] Invalid %s value \"%s\", using default (%d)", name, v, def)
	return def
}

func loadConfig() *Config {
	port := os.Getenv("PORT")
	if port == "" {
		port = "4050"
	}

	iconSource := os.Getenv("ICON_SOURCE")
	if iconSource == "" {
		iconSource = "remote"
	}

	remoteURL := strings.TrimRight(os.Getenv("REMOTE_URL"), "/")
	if remoteURL == "" {
		remoteURL = "https://cdn.jsdelivr.net/gh/selfhst/icons@main"
	}

	primaryColor := strings.TrimPrefix(os.Getenv("PRIMARY_COLOR"), "#")

	cacheTTL := time.Duration(parseIntEnv("CACHE_TTL", 3600)) * time.Second
	cacheSize := parseIntEnv("CACHE_SIZE", 500)
	remoteTimeout := time.Duration(parseIntEnv("REMOTE_TIMEOUT", 10)) * time.Second

	corsAllowedOrigins := os.Getenv("CORS_ALLOWED_ORIGINS")
	if corsAllowedOrigins == "" {
		corsAllowedOrigins = "*"
	}
	var corsOrigins []string
	for o := range strings.SplitSeq(corsAllowedOrigins, ",") {
		if trimmed := strings.TrimSpace(o); trimmed != "" {
			corsOrigins = append(corsOrigins, trimmed)
		}
	}

	logLevel := logLevelInfo
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		switch strings.ToLower(v) {
		case "debug":
			logLevel = logLevelDebug
		case "info":
			logLevel = logLevelInfo
		case "error":
			logLevel = logLevelError
		default:
			log.Printf("[WARN] Invalid LOG_LEVEL value \"%s\", using default (info)", v)
		}
	}

	return &Config{
		Port:          port,
		IconSource:    iconSource,
		RemoteURL:     remoteURL,
		LocalPath:     "/app/icons",
		PrimaryColor:  primaryColor,
		CacheTTL:      cacheTTL,
		CacheSize:     cacheSize,
		RemoteTimeout: remoteTimeout,
		CORSOrigins:   corsOrigins,
		LogLevel:      logLevel,
	}
}

func validateConfig(cfg *Config) {
	if cfg.IconSource != "local" && cfg.IconSource != "remote" && cfg.IconSource != "hybrid" {
		log.Fatalf("[ERROR] Invalid ICON_SOURCE \"%s\": must be \"local\", \"remote\", or \"hybrid\"", cfg.IconSource)
	}
	if cfg.IconSource == "local" || cfg.IconSource == "hybrid" {
		info, err := os.Stat(cfg.LocalPath)
		if err != nil {
			log.Fatalf("[ERROR] Icon path \"%s\" is not accessible: %v", cfg.LocalPath, err)
		}
		if !info.IsDir() {
			log.Fatalf("[ERROR] Icon path \"%s\" is not a directory", cfg.LocalPath)
		}
	}
	if cfg.PrimaryColor != "" && !isValidHexColor(cfg.PrimaryColor) {
		log.Fatalf("[ERROR] PRIMARY_COLOR \"%s\" is not a valid 6-digit hex color", cfg.PrimaryColor)
	}
	for _, origin := range cfg.CORSOrigins {
		if origin != "*" && !strings.HasPrefix(origin, "http://") && !strings.HasPrefix(origin, "https://") {
			log.Printf("[WARN] CORS_ALLOWED_ORIGINS entry \"%s\" is missing a scheme, did you mean \"https://%s\"?", origin, origin)
		}
	}
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c|0x20 && c|0x20 <= 'f'
}

func isValidHexColor(color string) bool {
	if len(color) != 6 {
		return false
	}
	for i := range len(color) {
		if !isHex(color[i]) {
			return false
		}
	}
	return true
}

func hexRGB(code string) (int64, int64, int64) {
	r, _ := strconv.ParseInt(code[0:2], 16, 0)
	g, _ := strconv.ParseInt(code[2:4], 16, 0)
	b, _ := strconv.ParseInt(code[4:6], 16, 0)
	return r, g, b
}

func newWhite(value, hexColor, code string) string {
	v := strings.ToLower(value)
	switch {
	case strings.HasPrefix(v, "#"):
		h := v[1:]
		switch len(h) {
		case 4:
			return hexColor + strings.Repeat(string(h[3]), 2)
		case 7:
			return hexColor + strings.Repeat(string(h[6]), 2)
		case 8:
			return hexColor + h[6:8]
		default:
			return hexColor
		}
	case strings.HasPrefix(v, "rgba("):
		_, inner, _ := strings.Cut(value, "(")
		inner, _, _ = strings.CutLast(inner, ")")
		parts := strings.FieldsFunc(inner, func(r rune) bool {
			return r == ',' || r == '/' || r == ' ' || r == '\t'
		})
		if len(parts) >= 4 {
			r, g, b := hexRGB(code)
			return fmt.Sprintf("rgba(%d, %d, %d, %s)", r, g, b, parts[len(parts)-1])
		}
		return hexColor
	default:
		return hexColor
	}
}

func applySVGColor(svg, colorCode string) string {
	hexColor := "#" + colorCode
	var b strings.Builder
	last := 0
	for i := 0; i < len(svg); i++ {
		c := svg[i] | 0x20
		if c != 'f' && c != 's' {
			continue
		}
		start, end, ok := matchWhiteAt(svg, i)
		if !ok {
			continue
		}
		if last == 0 {
			b.Grow(len(svg) + 64)
		}
		b.WriteString(svg[last:start])
		b.WriteString(newWhite(svg[start:end], hexColor, colorCode))
		last = end
		i = end
	}
	if last == 0 {
		return svg
	}
	b.WriteString(svg[last:])
	return b.String()
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\f' || c == '\r'
}

func isQuote(c byte) bool { return c == '"' || c == '\'' }

func isCSSEnd(c byte) bool {
	return isSpace(c) || strings.IndexByte(`;}/),"'`, c) >= 0
}

func matchWhiteAt(s string, i int) (start, end int, ok bool) {
	j := i
	switch {
	case hasPrefixFold(s[j:], "fill"):
		j += len("fill")
	case hasPrefixFold(s[j:], "stop-color"):
		j += len("stop-color")
	default:
		return 0, 0, false
	}
	if j >= len(s) {
		return 0, 0, false
	}
	switch s[j] {
	case '=':
		j++
		if j >= len(s) || !isQuote(s[j]) {
			return 0, 0, false
		}
		start = j + 1
		end = whiteEnd(s, start)
		if end < 0 || end >= len(s) || !isQuote(s[end]) {
			return 0, 0, false
		}
	case ':':
		j++
		for j < len(s) && isSpace(s[j]) {
			j++
		}
		start = j
		end = whiteEnd(s, start)
		if end < 0 || end < len(s) && !isCSSEnd(s[end]) {
			return 0, 0, false
		}
	default:
		return 0, 0, false
	}
	return start, end, true
}

func whiteEnd(s string, i int) int {
	if i >= len(s) {
		return -1
	}
	switch {
	case s[i] == '#':
		j := i + 1
		for j < len(s) && isHex(s[j]) {
			j++
		}
		h := s[i+1 : j]
		switch {
		case (len(h) == 3 || len(h) == 4) && strings.EqualFold(h[:3], "fff"),
			len(h) >= 6 && len(h) <= 8 && strings.EqualFold(h[:6], "ffffff"):
			return j
		}
		return -1
	case hasPrefixFold(s[i:], "white"):
		return i + len("white")
	case s[i]|0x20 == 'r':
		return rgbWhiteEnd(s, i)
	}
	return -1
}

func rgbWhiteEnd(s string, i int) int {
	j := i
	switch {
	case hasPrefixFold(s[j:], "rgba("):
		j += len("rgba(")
	case hasPrefixFold(s[j:], "rgb("):
		j += len("rgb(")
	default:
		return -1
	}
	skip := func() {
		for j < len(s) && isSpace(s[j]) {
			j++
		}
	}
	for n := range 3 {
		skip()
		if !strings.HasPrefix(s[j:], "255") {
			return -1
		}
		j += 3
		if n == 2 {
			break
		}

		from := j
		skip()
		if j < len(s) && s[j] == ',' {
			j++
		}
		skip()
		if j == from {
			return -1
		}
	}
	skip()
	if j < len(s) && (s[j] == ',' || s[j] == '/') {
		j++
		skip()
		from := j
		for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '.') {
			j++
		}
		if j == from {
			return -1
		}
		if j < len(s) && s[j] == '%' {
			j++
		}
		skip()
	}
	if j >= len(s) || s[j] != ')' {
		return -1
	}
	return j + 1
}

func getContentType(format string) string {
	switch format {
	case "png":
		return "image/png"
	case "webp":
		return webpType
	case "avif":
		return "image/avif"
	case "ico":
		return "image/x-icon"
	case "svg":
		return svgType
	case "jpg", "jpeg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	default:
		return ""
	}
}

func getCacheKey(iconName, colorCode string) string {
	if colorCode == "" {
		return iconName + ":default"
	}
	return iconName + ":" + colorCode
}

func acceptsGzip(header string) bool {
	for part := range strings.SplitSeq(header, ",") {
		coding, params, _ := strings.Cut(part, ";")
		coding = strings.TrimSpace(coding)
		if !strings.EqualFold(coding, "gzip") && !strings.EqualFold(coding, "x-gzip") {
			continue
		}
		for p := range strings.SplitSeq(params, ";") {
			k, v, _ := strings.Cut(p, "=")
			if strings.EqualFold(strings.TrimSpace(k), "q") {
				if q, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && q == 0 {
					return false
				}
			}
		}
		return true
	}
	return false
}

func etagMatches(header, etag string) bool {
	if header == "" {
		return false
	}
	for part := range strings.SplitSeq(header, ",") {
		t := strings.TrimSpace(part)
		if t == "*" || strings.TrimPrefix(t, "W/") == etag {
			return true
		}
	}
	return false
}

type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string { return fmt.Sprintf("HTTP %d", e.code) }

func isNotFound(err error) bool {
	if se, ok := errors.AsType[*httpStatusError](err); ok {
		return se.code == http.StatusNotFound || se.code == http.StatusGone
	}
	return errors.Is(err, fs.ErrNotExist)
}

type source struct {
	name     string
	get      func(dir, file string) ([]byte, error)
	upstream bool
}

type flight struct {
	mu    sync.Mutex
	calls map[string]*call
}

type call struct {
	wg  sync.WaitGroup
	res result
}

func (f *flight) do(key string, fn func() result) (res result, shared bool) {
	f.mu.Lock()
	if c, ok := f.calls[key]; ok {
		f.mu.Unlock()
		c.wg.Wait()
		return c.res, true
	}
	c := &call{}
	c.wg.Add(1)
	f.calls[key] = c
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		delete(f.calls, key)
		f.mu.Unlock()
		c.wg.Done()
	}()
	c.res = fn()
	return c.res, false
}

type result struct {
	status     int
	e          *entry
	format     string
	servedFrom string
}

type server struct {
	cfg          *Config
	cache        *Cache
	misses       *Cache
	flight       flight
	client       *http.Client
	sources      []source
	cacheControl string
}

func newServer(cfg *Config) *server {
	s := &server{
		cfg:          cfg,
		cache:        NewCache(cfg.CacheTTL, cfg.CacheSize),
		misses:       NewCache(min(cfg.CacheTTL, maxMissTTL), cfg.CacheSize),
		flight:       flight{calls: map[string]*call{}},
		client:       &http.Client{Timeout: cfg.RemoteTimeout},
		cacheControl: fmt.Sprintf("public, max-age=%d", int(cfg.CacheTTL.Seconds())),
	}
	if cfg.IconSource == "local" || cfg.IconSource == "hybrid" {
		s.sources = append(s.sources, source{"local", s.readLocal, false})
	}
	if cfg.IconSource == "remote" || cfg.IconSource == "hybrid" {
		s.sources = append(s.sources, source{"remote", s.fetchRemote, true})
	}
	return s
}

func (s *server) logf(level int, format string, args ...any) {
	if level >= s.cfg.LogLevel {
		log.Printf(format, args...)
	}
}

func (s *server) readLocal(dir, file string) ([]byte, error) {
	return os.ReadFile(filepath.Join(s.cfg.LocalPath, dir, file))
}

func (s *server) fetchRemote(dir, file string) ([]byte, error) {
	resp, err := s.client.Get(s.cfg.RemoteURL + "/" + dir + "/" + url.PathEscape(file))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, &httpStatusError{resp.StatusCode}
	}
	if resp.ContentLength > maxRemoteBody {
		return nil, fmt.Errorf("body of %d bytes is over the %d limit", resp.ContentLength, maxRemoteBody)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRemoteBody+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxRemoteBody {
		return nil, fmt.Errorf("body is over the %d byte limit", maxRemoteBody)
	}
	return data, nil
}

func (s *server) load(src source, dir, file string) (data []byte, failed bool) {
	data, err := src.get(dir, file)
	if err != nil {
		if isNotFound(err) {
			s.logf(logLevelDebug, "[DEBUG] Not found: %q (%s)", dir+"/"+file, src.name)
			return nil, false
		}
		s.logf(logLevelError, "[ERROR] Failed to read %q (%s): %v", dir+"/"+file, src.name, err)
		return nil, src.upstream
	}
	return data, false
}

func (s *server) resolve(baseName, format, colorCode string) result {
	contentType := getContentType(format)
	failed := false
	load := func(src source, dir, file string) []byte {
		b, f := s.load(src, dir, file)
		failed = failed || f
		return b
	}

	for _, src := range s.sources {
		var body []byte
		if colorCode != "" {
			if light := load(src, "svg", baseName+"-light.svg"); len(light) > 0 {
				body = []byte(applySVGColor(string(light), colorCode))
			}
		} else {
			body = load(src, format, baseName+"."+format)
			if len(body) == 0 && format != "webp" {
				if body = load(src, "webp", baseName+".webp"); len(body) > 0 {
					contentType = webpType
					format = "webp"
				}
			}
		}
		if len(body) > 0 {
			return result{status: http.StatusOK, e: newEntry(body, contentType, ""), format: format, servedFrom: src.name}
		}
	}
	if failed {
		return result{status: http.StatusBadGateway}
	}
	return result{status: http.StatusNotFound}
}

func (s *server) parseIconName(iconName string) (string, string) {
	ext := strings.ToLower(filepath.Ext(iconName))
	if ext != "" {
		format := strings.TrimPrefix(ext, ".")
		switch format {
		case "svg", "png", "webp", "avif", "ico":
			return strings.TrimSuffix(iconName, filepath.Ext(iconName)), format
		default:
			s.logf(logLevelError, "[WARN] Unrecognized extension %q in icon name %q, serving webp instead", ext, iconName)
			return strings.TrimSuffix(iconName, filepath.Ext(iconName)), "webp"
		}
	}
	return iconName, "webp"
}

func badName(name string) bool {
	return strings.Contains(name, "..") || strings.ContainsAny(name, `/\`)
}

func (s *server) notModified(w http.ResponseWriter, r *http.Request, etag, contentType string) bool {
	h := w.Header()
	h.Set("Cache-Control", s.cacheControl)
	h.Set("ETag", etag)
	if contentType == svgType {
		h.Add("Vary", "Accept-Encoding")
		h.Set("Content-Security-Policy", svgCSP)
	}
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	return false
}

func (s *server) writeEntry(w http.ResponseWriter, r *http.Request, e *entry, cacheStatus string) {
	w.Header().Set("X-Cache", cacheStatus)
	if s.notModified(w, r, e.etag, e.contentType) {
		return
	}
	h := w.Header()
	h.Set("Content-Type", e.contentType)
	body := e.body
	if e.gz != nil && acceptsGzip(r.Header.Get("Accept-Encoding")) {
		h.Set("Content-Encoding", "gzip")
		body = e.gz
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.Write(body)
}

func (s *server) handleIcon(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	iconName := r.PathValue("iconname")
	colorCode := r.PathValue("colorcode")

	if iconName == "" {
		http.Error(w, "Icon name is required", http.StatusBadRequest)
		return
	}

	baseName, format := s.parseIconName(iconName)
	baseName = strings.ToLower(baseName)

	if badName(baseName) {
		s.logf(logLevelError, "[ERROR] Invalid icon name, path traversal attempt: %q", iconName)
		http.Error(w, "Invalid icon name", http.StatusBadRequest)
		return
	}

	if colorCode == "" {
		colorCode = strings.TrimPrefix(r.URL.Query().Get("color"), "#")
	}

	primaryFallback := false
	if colorCode == "primary" {
		if s.cfg.PrimaryColor == "" {
			primaryFallback = true
		}
		colorCode = s.cfg.PrimaryColor
	}

	if colorCode != "" && !isValidHexColor(colorCode) {
		s.logf(logLevelError, "[ERROR] Invalid color code for icon %q: %q", baseName, colorCode)
		http.Error(w, "Invalid color code. Use 6-digit hex without #", http.StatusBadRequest)
		return
	}
	colorCode = strings.ToUpper(colorCode)

	formatToServe := format
	if colorCode != "" {
		formatToServe = "svg"
	}

	cacheKey := getCacheKey(baseName+"."+formatToServe, colorCode)

	var colorSuffix string
	if colorCode != "" {
		colorSuffix = " with color " + colorCode
	}

	if cached, found := s.cache.Get(cacheKey); found {
		s.logf(logLevelDebug, "[CACHE] Serving cached icon: %q%s (%s) %v", baseName, colorSuffix, formatToServe, formatDuration(time.Since(start)))
		s.writeEntry(w, r, cached, "HIT")
		return
	}
	if _, found := s.misses.Get(cacheKey); found {
		s.logf(logLevelDebug, "[CACHE] Icon not found (cached): %q%s %v", baseName, colorSuffix, formatDuration(time.Since(start)))
		w.Header().Set("X-Cache", "HIT")
		http.Error(w, "Icon not found", http.StatusNotFound)
		return
	}

	res, shared := s.flight.do(cacheKey, func() result {
		res := s.resolve(baseName, formatToServe, colorCode)
		switch res.status {
		case http.StatusOK:
			s.cache.Set(cacheKey, res.e)
		case http.StatusNotFound:
			s.misses.Set(cacheKey, &entry{})
		}
		return res
	})
	via := ""
	if shared {
		via = ", shared"
	}

	w.Header().Set("X-Cache", "MISS")
	switch res.status {
	case http.StatusNotFound:
		s.logf(logLevelError, "[ERROR] Icon not found: %q%s (source: %s%s) %v", baseName, colorSuffix, s.cfg.IconSource, via, formatDuration(time.Since(start)))
		http.Error(w, "Icon not found", http.StatusNotFound)
		return
	case http.StatusBadGateway:
		s.logf(logLevelError, "[ERROR] Upstream failed for icon: %q%s (source: %s%s) %v", baseName, colorSuffix, s.cfg.IconSource, via, formatDuration(time.Since(start)))
		http.Error(w, "Upstream error", http.StatusBadGateway)
		return
	}

	level := "SUCCESS"
	detail := colorSuffix
	if primaryFallback {
		level = "WARN"
		detail = " (PRIMARY_COLOR not set, using default format)"
	}
	s.logf(logLevelInfo, "[%s] Serving icon: %q%s (%s, source: %s%s) %v", level, baseName, detail, res.format, res.servedFrom, via, formatDuration(time.Since(start)))

	s.writeEntry(w, r, res.e, "MISS")
}

func (s *server) handleCustomIcon(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	filename := r.PathValue("filename")

	if filename == "" {
		http.Error(w, "Filename is required", http.StatusBadRequest)
		return
	}

	if badName(filename) {
		s.logf(logLevelError, "[ERROR] Invalid custom icon filename, path traversal attempt: %q", filename)
		http.Error(w, "Invalid filename", http.StatusBadRequest)
		return
	}

	filename = strings.ToLower(filename)
	customDir := filepath.Join(s.cfg.LocalPath, "custom")

	actualFilename := filename
	if entries, err := os.ReadDir(customDir); err == nil {
		for _, entry := range entries {
			if strings.ToLower(entry.Name()) == filename {
				actualFilename = entry.Name()
				break
			}
		}
	}

	customPath := filepath.Join(customDir, actualFilename)

	stat, err := os.Stat(customPath)
	if err != nil {
		if os.IsNotExist(err) {
			s.logf(logLevelError, "[ERROR] Custom icon not found: %q %v", filename, formatDuration(time.Since(start)))
			http.Error(w, "Custom icon not found", http.StatusNotFound)
		} else {
			s.logf(logLevelError, "[ERROR] Failed to read custom icon %q: %v (%v)", filename, err, formatDuration(time.Since(start)))
			http.Error(w, "Failed to read custom icon", http.StatusInternalServerError)
		}
		return
	}

	contentType := getContentType(strings.TrimPrefix(strings.ToLower(filepath.Ext(filename)), "."))
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	etag := fmt.Sprintf(`"%d-%d"`, stat.ModTime().Unix(), stat.Size())
	if s.notModified(w, r, etag, contentType) {
		return
	}

	cacheKey := "custom:" + filename + ":" + etag
	if cached, found := s.cache.Get(cacheKey); found {
		s.logf(logLevelDebug, "[CACHE] Serving cached custom icon: %q %v", filename, formatDuration(time.Since(start)))
		s.writeEntry(w, r, cached, "HIT")
		return
	}

	data, err := os.ReadFile(customPath)
	if err != nil {
		s.logf(logLevelError, "[ERROR] Failed to read custom icon %q: %v (%v)", filename, err, formatDuration(time.Since(start)))
		http.Error(w, "Failed to read custom icon", http.StatusInternalServerError)
		return
	}

	e := newEntry(data, contentType, etag)
	s.cache.Set(cacheKey, e)

	s.logf(logLevelInfo, "[SUCCESS] Serving custom icon: %q (%s) %v", filename, contentType, formatDuration(time.Since(start)))

	s.writeEntry(w, r, e, "MISS")
}

func headers(origins []string, next http.Handler) http.Handler {
	if len(origins) == 1 && origins[0] == "*" {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			next.ServeHTTP(w, r)
		})
	}
	allowed := make(map[string]bool, len(origins))
	for _, o := range origins {
		allowed[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Add("Vary", "Origin")
		if origin := r.Header.Get("Origin"); allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "selfh.st/icons\n\nEndpoints:\n  GET /{iconname}\n  GET /{iconname}/{colorcode}\n  GET /custom/{filename}\n  GET /health\n")
	})

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("GET /custom/{filename}", s.handleCustomIcon)

	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /{iconname}/{colorcode}", s.handleIcon)
	mux.HandleFunc("GET /{iconname}", s.handleIcon)

	return headers(s.cfg.CORSOrigins, mux)
}

func healthcheck() int {
	port := os.Getenv("PORT")
	if port == "" {
		port = "4050"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://localhost:" + port + "/health")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(healthcheck())
	}

	config := loadConfig()
	validateConfig(config)
	s := newServer(config)

	log.Printf("Icon server listening on port %s", config.Port)
	log.Printf("Icon source: %s", func() string {
		switch config.IconSource {
		case "local":
			return "Local volume"
		case "hybrid":
			return "Hybrid (local with remote fallback: " + config.RemoteURL + ")"
		default:
			return "Remote: " + config.RemoteURL
		}
	}())
	log.Printf("Cache settings: TTL %ds, Max %d items", int(config.CacheTTL.Seconds()), config.CacheSize)
	log.Printf("Log level: %s", []string{"debug", "info", "error"}[config.LogLevel])

	server := &http.Server{
		Addr:         ":" + config.Port,
		Handler:      s.routes(),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	cleanupCtx, cleanupCancel := context.WithCancel(context.Background())
	go func() {
		ticker := time.NewTicker(config.CacheTTL)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.cache.cleanup()
				s.misses.cleanup()
			case <-cleanupCtx.Done():
				return
			}
		}
	}()

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	<-quit
	cleanupCancel()
	log.Println("Shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}

	log.Println("Server stopped. Bye for now!")
}
