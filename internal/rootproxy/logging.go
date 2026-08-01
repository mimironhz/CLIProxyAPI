package rootproxy

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	sharedlogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	log "github.com/sirupsen/logrus"
	"gopkg.in/natefinch/lumberjack.v2"
)

const (
	rootApplicationLogName    = "root.log"
	rootAccessLogName         = "access.ndjson"
	rootStockLogName          = "stock-traffic.ndjson"
	rootLogCleanerInterval    = time.Minute
	rootStockPayloadPartBytes = 256 << 10
	rootStockTextPartBytes    = 128 << 10
	rootStockTrafficSchemaV1  = "root.stock-traffic.v1"
	rootStockTrafficSchemaV2  = "root.stock-traffic.v2"
)

var (
	rootAppOutputMu    sync.Mutex
	rootAppOutputOwner *rootLogManager
	rootRotatedLogName = regexp.MustCompile(`^(?:root-[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}-[0-9]{2}-[0-9]{2}\.[0-9]{3}\.log|(?:access|stock-traffic)-[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}-[0-9]{2}-[0-9]{2}\.[0-9]{3}\.ndjson)(?:\.gz)?$`)
)

type rootLogManager struct {
	directory string
	config    LoggingConfig

	appWriter    logWriteCloser
	accessWriter logWriteCloser
	stockWriter  logWriteCloser
	previousOut  io.Writer

	writeMu           sync.Mutex
	exchangeCounter   atomic.Uint64
	accessFailureOnce sync.Once
	stockFailureOnce  sync.Once
	cleanerStop       chan struct{}
	cleanerDone       chan struct{}
	closeOnce         sync.Once
}

type logWriteCloser interface {
	io.Writer
	io.Closer
}

type accessMetadata struct {
	mu sync.Mutex

	transport       string
	route           string
	model           string
	endpoint        string
	upstreamStatus  int
	requestBytes    int64
	hasRequestBytes bool
	responseBytes   int64
	outcome         string
	detail          string
	closeCode       int
}

type accessMetadataKey struct{}

type accessResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

type accessFlushingResponseWriter struct {
	*accessResponseWriter
}

type accessHijackingResponseWriter struct {
	*accessResponseWriter
}

type accessFlushingHijackingResponseWriter struct {
	*accessResponseWriter
}

type rootAccessRecord struct {
	Schema         string  `json:"schema"`
	Timestamp      string  `json:"ts"`
	RequestID      string  `json:"request_id"`
	Method         string  `json:"method"`
	Path           string  `json:"path"`
	Transport      string  `json:"transport,omitempty"`
	Route          string  `json:"route,omitempty"`
	Model          string  `json:"model,omitempty"`
	Endpoint       string  `json:"endpoint,omitempty"`
	Status         int     `json:"status"`
	UpstreamStatus int     `json:"upstream_status,omitempty"`
	RequestBytes   int64   `json:"request_bytes"`
	ResponseBytes  int64   `json:"response_bytes"`
	DurationMillis float64 `json:"duration_ms"`
	Outcome        string  `json:"outcome"`
	Detail         string  `json:"detail,omitempty"`
	CloseCode      int     `json:"websocket_close_code,omitempty"`
}

type stockExchange struct {
	manager    *rootLogManager
	requestID  string
	exchangeID string
	transport  string
	endpoint   string
	model      string

	mu             sync.Mutex
	sequence       uint64
	payloadCounter uint64
	captureFailed  bool
	closed         bool
}

func newRootLogManager(config *Config) (*rootLogManager, error) {
	if config == nil || !config.Logging.LoggingToFile {
		return nil, nil
	}
	directory, errDirectory := config.logDirectory()
	if errDirectory != nil {
		return nil, errDirectory
	}
	if errPrepare := preparePrivateLogDirectory(directory); errPrepare != nil {
		return nil, errPrepare
	}

	manager := &rootLogManager{
		directory:   directory,
		config:      config.Logging,
		cleanerStop: make(chan struct{}),
		cleanerDone: make(chan struct{}),
	}
	appPath := filepath.Join(directory, rootApplicationLogName)
	if errPrepare := preparePrivateLogFile(appPath); errPrepare != nil {
		return nil, errPrepare
	}
	manager.appWriter = manager.newRotatingWriter(appPath)

	if config.Logging.RequestAccessLog {
		accessPath := filepath.Join(directory, rootAccessLogName)
		if errPrepare := preparePrivateLogFile(accessPath); errPrepare != nil {
			_ = manager.appWriter.Close()
			return nil, errPrepare
		}
		manager.accessWriter = manager.newRotatingWriter(accessPath)
	}
	if config.Logging.StockRequestResponseLog {
		stockPath := filepath.Join(directory, rootStockLogName)
		if errPrepare := preparePrivateLogFile(stockPath); errPrepare != nil {
			_ = manager.closeWriters()
			return nil, errPrepare
		}
		manager.stockWriter = manager.newRotatingWriter(stockPath)
	}

	rootAppOutputMu.Lock()
	if rootAppOutputOwner != nil {
		rootAppOutputMu.Unlock()
		_ = manager.closeWriters()
		return nil, errors.New("root proxy file logging is already active")
	}
	manager.previousOut = log.StandardLogger().Out
	log.SetOutput(manager.appWriter)
	rootAppOutputOwner = manager
	rootAppOutputMu.Unlock()

	manager.prune()
	go manager.runCleaner()
	return manager, nil
}

func (m *rootLogManager) newRotatingWriter(path string) *lumberjack.Logger {
	return &lumberjack.Logger{
		Filename:   path,
		MaxSize:    m.config.MaxFileSizeMB,
		MaxBackups: m.config.MaxBackups,
		MaxAge:     m.config.MaxAgeDays,
		Compress:   m.config.Compress,
	}
}

func preparePrivateLogDirectory(path string) error {
	info, errStat := os.Lstat(path)
	if errors.Is(errStat, os.ErrNotExist) {
		if errCreate := os.MkdirAll(path, 0o700); errCreate != nil {
			return fmt.Errorf("create root log directory: %w", errCreate)
		}
		if errMode := os.Chmod(path, 0o700); errMode != nil {
			return fmt.Errorf("secure root log directory: %w", errMode)
		}
		info, errStat = os.Lstat(path)
	}
	if errStat != nil {
		return fmt.Errorf("stat root log directory: %w", errStat)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("root log directory must be a real directory, not a symlink")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("root log directory permissions %04o are too open; require 0700", info.Mode().Perm())
	}
	return nil
}

func preparePrivateLogFile(path string) error {
	info, errStat := os.Lstat(path)
	if errStat == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("root log file %s must be a regular file, not a symlink", filepath.Base(path))
		}
		if errMode := os.Chmod(path, 0o600); errMode != nil {
			return fmt.Errorf("secure root log file %s: %w", filepath.Base(path), errMode)
		}
	} else if !errors.Is(errStat, os.ErrNotExist) {
		return fmt.Errorf("stat root log file %s: %w", filepath.Base(path), errStat)
	}
	file, errOpen := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if errOpen != nil {
		return fmt.Errorf("open root log file %s: %w", filepath.Base(path), errOpen)
	}
	if errClose := file.Close(); errClose != nil {
		return fmt.Errorf("close root log file %s: %w", filepath.Base(path), errClose)
	}
	return nil
}

func (m *rootLogManager) close() {
	if m == nil {
		return
	}
	m.closeOnce.Do(func() {
		close(m.cleanerStop)
		<-m.cleanerDone

		rootAppOutputMu.Lock()
		if rootAppOutputOwner == m {
			log.SetOutput(m.previousOut)
			rootAppOutputOwner = nil
		}
		rootAppOutputMu.Unlock()

		if errClose := m.closeWriters(); errClose != nil {
			log.WithError(errClose).Debug("root proxy failed to close log files")
		}
	})
}

func (m *rootLogManager) closeWriters() error {
	if m == nil {
		return nil
	}
	var closeErrors []error
	for _, writer := range []logWriteCloser{m.stockWriter, m.accessWriter, m.appWriter} {
		if writer == nil {
			continue
		}
		if errClose := writer.Close(); errClose != nil {
			closeErrors = append(closeErrors, errClose)
		}
	}
	return errors.Join(closeErrors...)
}

func (m *rootLogManager) runCleaner() {
	defer close(m.cleanerDone)
	ticker := time.NewTicker(rootLogCleanerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.prune()
		case <-m.cleanerStop:
			m.prune()
			return
		}
	}
}

type rootOwnedLogFile struct {
	path    string
	size    int64
	modTime time.Time
}

func (m *rootLogManager) prune() {
	if m == nil || m.config.MaxTotalSizeMB <= 0 {
		return
	}
	entries, errRead := os.ReadDir(m.directory)
	if errRead != nil {
		return
	}
	protected := map[string]struct{}{rootApplicationLogName: {}}
	if m.accessWriter != nil {
		protected[rootAccessLogName] = struct{}{}
	}
	if m.stockWriter != nil {
		protected[rootStockLogName] = struct{}{}
	}
	var total int64
	candidates := make([]rootOwnedLogFile, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.Type()&os.ModeSymlink != 0 || !rootOwnsLogFilename(name) {
			continue
		}
		info, errInfo := entry.Info()
		if errInfo != nil || !info.Mode().IsRegular() {
			continue
		}
		total += info.Size()
		if _, keep := protected[name]; keep {
			continue
		}
		candidates = append(candidates, rootOwnedLogFile{
			path:    filepath.Join(m.directory, name),
			size:    info.Size(),
			modTime: info.ModTime(),
		})
	}
	limit := int64(m.config.MaxTotalSizeMB) << 20
	if total <= limit {
		return
	}
	sort.Slice(candidates, func(left, right int) bool {
		if candidates[left].modTime.Equal(candidates[right].modTime) {
			return candidates[left].path < candidates[right].path
		}
		return candidates[left].modTime.Before(candidates[right].modTime)
	})
	for _, candidate := range candidates {
		if total <= limit {
			break
		}
		if errRemove := os.Remove(candidate.path); errRemove == nil {
			total -= candidate.size
		}
	}
}

func rootOwnsLogFilename(name string) bool {
	for _, current := range []string{rootApplicationLogName, rootAccessLogName, rootStockLogName} {
		if name == current {
			return true
		}
	}
	return rootRotatedLogName.MatchString(name)
}

func (m *rootLogManager) accessMiddleware(next http.Handler) http.Handler {
	if m == nil || m.accessWriter == nil {
		return next
	}
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		started := time.Now()
		requestID := sharedlogging.GenerateRequestID()
		metadata := &accessMetadata{transport: "http"}
		ctx := sharedlogging.WithRequestID(request.Context(), requestID)
		ctx = context.WithValue(ctx, accessMetadataKey{}, metadata)
		request = request.WithContext(ctx)
		wrapped, tracker := wrapAccessResponseWriter(response)

		defer func() {
			status := tracker.status
			if status == 0 {
				status = http.StatusOK
			}
			metadata.mu.Lock()
			transport := metadata.transport
			routeName := metadata.route
			model := metadata.model
			endpoint := metadata.endpoint
			upstreamStatus := metadata.upstreamStatus
			requestBytes := metadata.requestBytes
			applicationResponseBytes := metadata.responseBytes
			explicitOutcome := metadata.outcome
			detail := metadata.detail
			closeCode := metadata.closeCode
			if !metadata.hasRequestBytes && request.ContentLength > 0 {
				requestBytes = request.ContentLength
			}
			metadata.mu.Unlock()

			outcome := "completed"
			switch {
			case request.Context().Err() != nil:
				outcome = "canceled"
			case status >= http.StatusInternalServerError:
				outcome = "failed"
			case status >= http.StatusBadRequest:
				outcome = "rejected"
			}
			if explicitOutcome != "" {
				outcome = explicitOutcome
			}
			record := rootAccessRecord{
				Schema:         "root.access.v1",
				Timestamp:      time.Now().UTC().Format(time.RFC3339Nano),
				RequestID:      requestID,
				Method:         request.Method,
				Path:           request.URL.Path,
				Transport:      transport,
				Route:          routeName,
				Model:          model,
				Endpoint:       endpoint,
				Status:         status,
				UpstreamStatus: upstreamStatus,
				RequestBytes:   requestBytes,
				ResponseBytes:  tracker.bytes + applicationResponseBytes,
				DurationMillis: float64(time.Since(started).Microseconds()) / 1000,
				Outcome:        outcome,
				Detail:         detail,
				CloseCode:      closeCode,
			}
			m.writeAccessRecord(record)
		}()

		next.ServeHTTP(wrapped, request)
	})
}

func wrapAccessResponseWriter(response http.ResponseWriter) (http.ResponseWriter, *accessResponseWriter) {
	tracker := &accessResponseWriter{ResponseWriter: response}
	_, canFlush := response.(http.Flusher)
	_, canHijack := response.(http.Hijacker)
	switch {
	case canFlush && canHijack:
		return &accessFlushingHijackingResponseWriter{accessResponseWriter: tracker}, tracker
	case canFlush:
		return &accessFlushingResponseWriter{accessResponseWriter: tracker}, tracker
	case canHijack:
		return &accessHijackingResponseWriter{accessResponseWriter: tracker}, tracker
	default:
		return tracker, tracker
	}
}

func setAccessSelection(ctx context.Context, transport, endpoint string, selected route, model string, requestBytes int64) {
	metadata, _ := ctx.Value(accessMetadataKey{}).(*accessMetadata)
	if metadata == nil {
		return
	}
	metadata.mu.Lock()
	metadata.transport = transport
	metadata.endpoint = endpoint
	metadata.route = selected.String()
	metadata.model = model
	metadata.requestBytes = requestBytes
	metadata.hasRequestBytes = true
	metadata.mu.Unlock()
}

func updateAccessSelection(ctx context.Context, transport, endpoint string, selected route, model string) {
	metadata, _ := ctx.Value(accessMetadataKey{}).(*accessMetadata)
	if metadata == nil {
		return
	}
	metadata.mu.Lock()
	metadata.transport = transport
	metadata.endpoint = endpoint
	metadata.route = selected.String()
	metadata.model = model
	metadata.mu.Unlock()
}

func setAccessTransport(ctx context.Context, transport, endpoint string) {
	metadata, _ := ctx.Value(accessMetadataKey{}).(*accessMetadata)
	if metadata == nil {
		return
	}
	metadata.mu.Lock()
	metadata.transport = transport
	metadata.endpoint = endpoint
	metadata.mu.Unlock()
}

func setAccessUpstreamStatus(ctx context.Context, status int) {
	metadata, _ := ctx.Value(accessMetadataKey{}).(*accessMetadata)
	if metadata == nil {
		return
	}
	metadata.mu.Lock()
	metadata.upstreamStatus = status
	metadata.mu.Unlock()
}

func addAccessRequestBytes(ctx context.Context, count int) {
	metadata, _ := ctx.Value(accessMetadataKey{}).(*accessMetadata)
	if metadata == nil || count <= 0 {
		return
	}
	metadata.mu.Lock()
	metadata.requestBytes += int64(count)
	metadata.hasRequestBytes = true
	metadata.mu.Unlock()
}

func setAccessRequestBytes(ctx context.Context, count int64) {
	metadata, _ := ctx.Value(accessMetadataKey{}).(*accessMetadata)
	if metadata == nil || count < 0 {
		return
	}
	metadata.mu.Lock()
	metadata.requestBytes = count
	metadata.hasRequestBytes = true
	metadata.mu.Unlock()
}

func addAccessResponseBytes(ctx context.Context, count int) {
	metadata, _ := ctx.Value(accessMetadataKey{}).(*accessMetadata)
	if metadata == nil || count <= 0 {
		return
	}
	metadata.mu.Lock()
	metadata.responseBytes += int64(count)
	metadata.mu.Unlock()
}

func setAccessOutcome(ctx context.Context, outcome, detail string, closeCode int) {
	metadata, _ := ctx.Value(accessMetadataKey{}).(*accessMetadata)
	if metadata == nil {
		return
	}
	metadata.mu.Lock()
	metadata.outcome = outcome
	metadata.detail = detail
	metadata.closeCode = closeCode
	metadata.mu.Unlock()
}

func setAccessOutcomeIfUnset(ctx context.Context, outcome, detail string, closeCode int) {
	metadata, _ := ctx.Value(accessMetadataKey{}).(*accessMetadata)
	if metadata == nil {
		return
	}
	metadata.mu.Lock()
	if metadata.outcome == "" {
		metadata.outcome = outcome
		metadata.detail = detail
		metadata.closeCode = closeCode
	}
	metadata.mu.Unlock()
}

func (w *accessResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *accessResponseWriter) Write(payload []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	written, errWrite := w.ResponseWriter.Write(payload)
	w.bytes += int64(written)
	return written, errWrite
}

func (w *accessResponseWriter) flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	w.ResponseWriter.(http.Flusher).Flush()
}

func (w *accessResponseWriter) hijack() (net.Conn, *bufio.ReadWriter, error) {
	connection, buffer, errHijack := w.ResponseWriter.(http.Hijacker).Hijack()
	if errHijack == nil && w.status == 0 {
		w.status = http.StatusSwitchingProtocols
	}
	return connection, buffer, errHijack
}

func (w *accessFlushingResponseWriter) Flush() {
	w.accessResponseWriter.flush()
}

func (w *accessHijackingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.accessResponseWriter.hijack()
}

func (w *accessFlushingHijackingResponseWriter) Flush() {
	w.accessResponseWriter.flush()
}

func (w *accessFlushingHijackingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.accessResponseWriter.hijack()
}

func (w *accessResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (m *rootLogManager) writeAccessRecord(record rootAccessRecord) {
	if m == nil || m.accessWriter == nil {
		return
	}
	if errWrite := m.writeJSON(m.accessWriter, record); errWrite != nil {
		m.accessFailureOnce.Do(func() {
			log.WithError(errWrite).Error("root proxy access logging failed; proxying continues")
		})
	}
}

func (m *rootLogManager) beginStockExchange(ctx context.Context, selected route, transport, endpoint, model string) *stockExchange {
	if m == nil || m.stockWriter == nil || selected != routeOfficial {
		return nil
	}
	requestID := sharedlogging.GetRequestID(ctx)
	if requestID == "" {
		requestID = sharedlogging.GenerateRequestID()
	}
	ordinal := m.exchangeCounter.Add(1)
	exchange := &stockExchange{
		manager:    m,
		requestID:  requestID,
		exchangeID: fmt.Sprintf("%s-%06d", requestID, ordinal),
		transport:  transport,
		endpoint:   endpoint,
		model:      model,
	}
	exchange.writeEvent(map[string]any{
		"kind": "begin",
	})
	return exchange
}

func (e *stockExchange) recordPayload(kind, direction string, payload []byte, fields map[string]any) {
	if e == nil {
		return
	}
	wholeHash := sha256.Sum256(payload)
	textPayload := e.manager.config.StockPayloadFormat == stockPayloadFormatAuto && stockPayloadCanUseText(payload, fields)
	parts := splitStockPayload(payload, textPayload)

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	e.payloadCounter++
	payloadID := fmt.Sprintf("%s-p%06d", e.exchangeID, e.payloadCounter)
	for partIndex, bounds := range parts {
		start := bounds.start
		end := bounds.end
		part := payload[start:end]
		partHash := sha256.Sum256(part)
		e.sequence++
		event := make(map[string]any, len(fields)+16)
		for key, value := range fields {
			event[key] = value
		}
		for key, value := range e.baseEventLocked() {
			event[key] = value
		}
		event["kind"] = kind
		event["direction"] = direction
		event["payload_id"] = payloadID
		if textPayload {
			event["payload_encoding"] = "utf-8"
			event["payload_text"] = string(part)
		} else {
			event["payload_encoding"] = "base64"
			event["payload_base64"] = base64.StdEncoding.EncodeToString(part)
		}
		event["payload_bytes"] = len(payload)
		event["payload_sha256"] = hex.EncodeToString(wholeHash[:])
		event["payload_part"] = partIndex + 1
		event["payload_parts"] = len(parts)
		event["payload_offset"] = start
		event["payload_part_bytes"] = len(part)
		event["payload_part_sha256"] = hex.EncodeToString(partHash[:])
		if errWrite := e.manager.writeStockEvent(event); errWrite != nil {
			e.captureFailed = true
		}
	}
}

type stockPayloadPart struct {
	start int
	end   int
}

func splitStockPayload(payload []byte, textPayload bool) []stockPayloadPart {
	if len(payload) == 0 {
		return []stockPayloadPart{{}}
	}
	partBytes := rootStockPayloadPartBytes
	if textPayload {
		partBytes = rootStockTextPartBytes
	}
	parts := make([]stockPayloadPart, 0, (len(payload)+partBytes-1)/partBytes)
	for start := 0; start < len(payload); {
		end := min(start+partBytes, len(payload))
		if textPayload && end < len(payload) {
			for end > start && !utf8.Valid(payload[start:end]) {
				end--
			}
		}
		parts = append(parts, stockPayloadPart{start: start, end: end})
		start = end
	}
	return parts
}

func stockPayloadCanUseText(payload []byte, fields map[string]any) bool {
	if !utf8.Valid(payload) {
		return false
	}
	contentEncoding, _ := fields["content_encoding"].(string)
	contentEncoding = strings.ToLower(strings.TrimSpace(contentEncoding))
	if contentEncoding != "" && contentEncoding != "identity" {
		return false
	}
	if opcode, ok := fields["opcode"].(string); ok && opcode != "text" {
		return false
	}
	return true
}

func (e *stockExchange) finish(outcome string, terminalCaptured bool, detail string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	e.closed = true
	e.sequence++
	event := e.baseEventLocked()
	event["kind"] = "end"
	event["outcome"] = outcome
	event["capture_complete"] = terminalCaptured && !e.captureFailed
	if e.captureFailed {
		event["capture_error"] = "one_or_more_log_records_failed"
	}
	if detail != "" {
		event["detail"] = detail
	}
	if errWrite := e.manager.writeStockEvent(event); errWrite != nil {
		e.captureFailed = true
	}
}

func (e *stockExchange) writeEvent(fields map[string]any) {
	if e == nil || e.manager == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	e.sequence++
	event := make(map[string]any, len(fields)+8)
	for key, value := range fields {
		event[key] = value
	}
	for key, value := range e.baseEventLocked() {
		event[key] = value
	}
	if errWrite := e.manager.writeStockEvent(event); errWrite != nil {
		e.captureFailed = true
	}
}

func (e *stockExchange) baseEventLocked() map[string]any {
	schema := rootStockTrafficSchemaV1
	if e.manager.config.StockPayloadFormat == stockPayloadFormatAuto {
		schema = rootStockTrafficSchemaV2
	}
	return map[string]any{
		"schema":      schema,
		"ts":          time.Now().UTC().Format(time.RFC3339Nano),
		"request_id":  e.requestID,
		"exchange_id": e.exchangeID,
		"seq":         e.sequence,
		"transport":   e.transport,
		"endpoint":    e.endpoint,
		"model":       e.model,
	}
}

func (m *rootLogManager) writeStockEvent(event map[string]any) error {
	if m == nil || m.stockWriter == nil {
		return nil
	}
	errWrite := m.writeJSON(m.stockWriter, event)
	if errWrite != nil {
		m.stockFailureOnce.Do(func() {
			log.WithError(errWrite).Error("root proxy stock traffic logging failed; proxying continues")
		})
	}
	return errWrite
}

func (m *rootLogManager) writeJSON(writer io.Writer, value any) error {
	payload, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return errMarshal
	}
	payload = append(payload, '\n')
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	written, errWrite := writer.Write(payload)
	if errWrite != nil {
		return errWrite
	}
	if written != len(payload) {
		return io.ErrShortWrite
	}
	return nil
}
