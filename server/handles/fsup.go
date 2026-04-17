package handles

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"net/url"
	"os"
	stdpath "path"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/internal/task"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/cespare/xxhash/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func getLastModified(c *gin.Context) time.Time {
	now := time.Now()
	lastModifiedStr := c.GetHeader("Last-Modified")
	lastModifiedMillisecond, err := strconv.ParseInt(lastModifiedStr, 10, 64)
	if err != nil {
		return now
	}
	lastModified := time.UnixMilli(lastModifiedMillisecond)
	return lastModified
}

// getUserFromContext extracts user from gin context
func getUserFromContext(c *gin.Context) *model.User {
	return c.Request.Context().Value(conf.UserKey).(*model.User)
}

// resolveUserPath resolves raw path to absolute path with user permission check
func resolveUserPath(c *gin.Context, rawPath string) (string, error) {
	user := getUserFromContext(c)
	return user.JoinPath(rawPath)
}

// checkFileExists checks if file exists when overwrite is not allowed
func checkFileExists(ctx context.Context, path string, overwrite bool) error {
	if overwrite {
		return nil
	}
	if res, _ := fs.Get(ctx, path, &fs.GetArgs{NoLog: true}); res != nil {
		return fmt.Errorf("file exists")
	}
	return nil
}

// checkWritePermission checks if user has write permission to the storage
func checkWritePermission(path string) error {
	storage, err := fs.GetStorage(path, &fs.GetStoragesArgs{})
	if err != nil {
		return err
	}
	if storage.Config().NoUpload {
		return fmt.Errorf("storage does not support upload")
	}
	return nil
}

// validateFileName checks if filename should be ignored as system file
func validateFileName(name string) error {
	if shouldIgnoreSystemFile(name) {
		return errs.IgnoredSystemFile
	}
	return nil
}

// shouldIgnoreSystemFile checks if the filename should be ignored based on settings
func shouldIgnoreSystemFile(filename string) bool {
	if setting.GetBool(conf.IgnoreSystemFiles) {
		return utils.IsSystemFile(filename)
	}
	return false
}

// StreamUploadSession manages a chunked stream upload session
type StreamUploadSession struct {
	pipeWriter *io.PipeWriter
	pipeReader *io.PipeReader
	totalSize  int64
	received   int64
	done       chan error
	lastActive time.Time
	mu         sync.Mutex
}

// streamUploadSessions stores active upload sessions
var streamUploadSessions = sync.Map{}

// cleanupInterval for expired sessions
const streamSessionTimeout = 10 * time.Minute

func init() {
	// Start cleanup goroutine for expired sessions
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			now := time.Now()
			streamUploadSessions.Range(func(key, value any) bool {
				session := value.(*StreamUploadSession)
				session.mu.Lock()
				if now.Sub(session.lastActive) > streamSessionTimeout {
					session.pipeWriter.CloseWithError(fmt.Errorf("session timeout"))
					streamUploadSessions.Delete(key)
				}
				session.mu.Unlock()
				return true
			})
		}
	}()
}

// parseContentRange parses Content-Range header: bytes start-end/total
func parseContentRange(header string) (start, end, total int64, err error) {
	re := regexp.MustCompile(`bytes (\d+)-(\d+)/(\d+)`)
	matches := re.FindStringSubmatch(header)
	if len(matches) != 4 {
		return 0, 0, 0, fmt.Errorf("invalid Content-Range format")
	}
	start, _ = strconv.ParseInt(matches[1], 10, 64)
	end, _ = strconv.ParseInt(matches[2], 10, 64)
	total, _ = strconv.ParseInt(matches[3], 10, 64)
	return
}

// generateStreamSessionKey creates a unique key for the upload session
func generateStreamSessionKey(userID uint, path string, totalSize int64) string {
	return fmt.Sprintf("stream:%d:%s:%d", userID, path, totalSize)
}

func FsStream(c *gin.Context) {
	defer func() {
		if n, _ := io.ReadFull(c.Request.Body, []byte{0}); n == 1 {
			_, _ = utils.CopyWithBuffer(io.Discard, c.Request.Body)
		}
		_ = c.Request.Body.Close()
	}()

	// Check for Content-Range header (chunked upload)
	contentRange := c.GetHeader("Content-Range")
	if contentRange != "" {
		fsStreamChunked(c, contentRange)
		return
	}

	// Original logic for non-chunked upload
	fsStreamDirect(c)
}

// fsStreamChunked handles chunked stream upload with Content-Range
func fsStreamChunked(c *gin.Context, contentRange string) {
	// Parse Content-Range: bytes start-end/total
	start, _, total, err := parseContentRange(contentRange)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}

	path := c.GetHeader("File-Path")
	path, err = url.PathUnescape(path)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}

	overwrite := c.GetHeader("Overwrite") != "false"
	user := c.Request.Context().Value(conf.UserKey).(*model.User)
	path, err = user.JoinPath(path)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}

	dir, name := stdpath.Split(path)
	if shouldIgnoreSystemFile(name) {
		common.ErrorStrResp(c, errs.IgnoredSystemFile.Error(), 403)
		return
	}

	// Generate session key
	sessionKey := generateStreamSessionKey(user.ID, path, total)

	if start == 0 {
		// First chunk: check overwrite and create session
		if !overwrite {
			if res, _ := fs.Get(c.Request.Context(), path, &fs.GetArgs{NoLog: true}); res != nil {
				common.ErrorStrResp(c, "file exists", 403)
				return
			}
		}

		// Create pipe for streaming
		pr, pw := io.Pipe()
		session := &StreamUploadSession{
			pipeWriter: pw,
			pipeReader: pr,
			totalSize:  total,
			received:   0,
			done:       make(chan error, 1),
			lastActive: time.Now(),
		}
		streamUploadSessions.Store(sessionKey, session)

		// Get mimetype
		mimetype := c.GetHeader("Content-Type")
		if len(mimetype) == 0 || mimetype == "application/octet-stream" {
			mimetype = utils.GetMimeType(name)
		}

		// Start upload goroutine - reads from pipe and uploads to storage
		go func() {
			s := &stream.FileStream{
				Obj: &model.Object{
					Name:     name,
					Size:     total,
					Modified: getLastModified(c),
				},
				Reader:       pr,
				Mimetype:     mimetype,
				WebPutAsTask: false, // Chunked upload is inherently async-safe
			}
			// Use background context since original request may complete
			err := fs.PutDirectly(context.Background(), dir, s, false)
			session.done <- err
		}()
	}

	// Get session
	sessionVal, ok := streamUploadSessions.Load(sessionKey)
	if !ok {
		common.ErrorStrResp(c, "upload session not found, please start from first chunk", 400)
		return
	}
	session := sessionVal.(*StreamUploadSession)

	session.mu.Lock()
	session.lastActive = time.Now()
	session.mu.Unlock()

	// Write request body to pipe (streaming - no buffering)
	written, err := io.Copy(session.pipeWriter, c.Request.Body)
	if err != nil {
		session.pipeWriter.CloseWithError(err)
		streamUploadSessions.Delete(sessionKey)
		common.ErrorResp(c, err, 500)
		return
	}

	session.mu.Lock()
	session.received += written
	currentReceived := session.received
	session.mu.Unlock()

	// Check if this is the last chunk
	if currentReceived >= total {
		// Close pipe to signal completion
		session.pipeWriter.Close()

		// Wait for upload to complete
		uploadErr := <-session.done
		streamUploadSessions.Delete(sessionKey)

		if uploadErr != nil {
			common.ErrorResp(c, uploadErr, 500)
			return
		}
	}

	common.SuccessResp(c, gin.H{
		"received": currentReceived,
		"total":    total,
		"complete": currentReceived >= total,
	})
}

// fsStreamDirect handles direct (non-chunked) stream upload
func fsStreamDirect(c *gin.Context) {
	path := c.GetHeader("File-Path")
	path, err := url.PathUnescape(path)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	asTask := c.GetHeader("As-Task") == "true"
	overwrite := c.GetHeader("Overwrite") != "false"
	user := c.Request.Context().Value(conf.UserKey).(*model.User)
	path, err = user.JoinPath(path)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	if !overwrite {
		if res, _ := fs.Get(c.Request.Context(), path, &fs.GetArgs{NoLog: true}); res != nil {
			common.ErrorStrResp(c, "file exists", 403)
			return
		}
	}
	dir, name := stdpath.Split(path)
	// Check if system file should be ignored
	if shouldIgnoreSystemFile(name) {
		common.ErrorStrResp(c, errs.IgnoredSystemFile.Error(), 403)
		return
	}
	// 如果请求头 Content-Length 和 X-File-Size 都没有，则 size=-1，表示未知大小的流式上传
	size := c.Request.ContentLength
	if size < 0 {
		sizeStr := c.GetHeader("X-File-Size")
		if sizeStr != "" {
			size, err = strconv.ParseInt(sizeStr, 10, 64)
			if err != nil {
				common.ErrorResp(c, err, 400)
				return
			}
		}
	}
	h := make(map[*utils.HashType]string)
	if md5 := c.GetHeader("X-File-Md5"); md5 != "" {
		h[utils.MD5] = md5
	}
	if sha1 := c.GetHeader("X-File-Sha1"); sha1 != "" {
		h[utils.SHA1] = sha1
	}
	if sha256 := c.GetHeader("X-File-Sha256"); sha256 != "" {
		h[utils.SHA256] = sha256
	}
	mimetype := c.GetHeader("Content-Type")
	if len(mimetype) == 0 {
		mimetype = utils.GetMimeType(name)
	}
	s := &stream.FileStream{
		Obj: &model.Object{
			Name:     name,
			Size:     size,
			Modified: getLastModified(c),
			HashInfo: utils.NewHashInfoByMap(h),
		},
		Reader:       c.Request.Body,
		Mimetype:     mimetype,
		WebPutAsTask: asTask,
	}
	var t task.TaskExtensionInfo
	if asTask {
		t, err = fs.PutAsTask(c.Request.Context(), dir, s)
	} else {
		err = fs.PutDirectly(c.Request.Context(), dir, s)
	}
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	if t == nil {
		common.SuccessResp(c)
		return
	}
	common.SuccessResp(c, gin.H{
		"task": getTaskInfo(t),
	})
}

func FsForm(c *gin.Context) {
	defer func() {
		if n, _ := io.ReadFull(c.Request.Body, []byte{0}); n == 1 {
			_, _ = utils.CopyWithBuffer(io.Discard, c.Request.Body)
		}
		_ = c.Request.Body.Close()
	}()
	path := c.GetHeader("File-Path")
	path, err := url.PathUnescape(path)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	asTask := c.GetHeader("As-Task") == "true"
	overwrite := c.GetHeader("Overwrite") != "false"
	user := c.Request.Context().Value(conf.UserKey).(*model.User)
	path, err = user.JoinPath(path)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	if !overwrite {
		if res, _ := fs.Get(c.Request.Context(), path, &fs.GetArgs{NoLog: true}); res != nil {
			common.ErrorStrResp(c, "file exists", 403)
			return
		}
	}
	storage, err := fs.GetStorage(path, &fs.GetStoragesArgs{})
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if storage.Config().NoUpload {
		common.ErrorStrResp(c, "Current storage doesn't support upload", 405)
		return
	}
	file, err := c.FormFile("file")
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	f, err := file.Open()
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	defer f.Close()
	dir, name := stdpath.Split(path)
	// Check if system file should be ignored
	if shouldIgnoreSystemFile(name) {
		common.ErrorStrResp(c, errs.IgnoredSystemFile.Error(), 403)
		return
	}
	h := make(map[*utils.HashType]string)
	if md5 := c.GetHeader("X-File-Md5"); md5 != "" {
		h[utils.MD5] = md5
	}
	if sha1 := c.GetHeader("X-File-Sha1"); sha1 != "" {
		h[utils.SHA1] = sha1
	}
	if sha256 := c.GetHeader("X-File-Sha256"); sha256 != "" {
		h[utils.SHA256] = sha256
	}
	mimetype := file.Header.Get("Content-Type")
	if len(mimetype) == 0 {
		mimetype = utils.GetMimeType(name)
	}
	s := &stream.FileStream{
		Obj: &model.Object{
			Name:     name,
			Size:     file.Size,
			Modified: getLastModified(c),
			HashInfo: utils.NewHashInfoByMap(h),
		},
		Reader:       f,
		Mimetype:     mimetype,
		WebPutAsTask: asTask,
	}
	var t task.TaskExtensionInfo
	if asTask {
		s.Reader = struct {
			io.Reader
		}{f}
		t, err = fs.PutAsTask(c.Request.Context(), dir, s)
	} else {
		err = fs.PutDirectly(c.Request.Context(), dir, s)
	}
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	if t == nil {
		common.SuccessResp(c)
		return
	}
	common.SuccessResp(c, gin.H{
		"task": getTaskInfo(t),
	})
}

type hashVerifyingReader struct {
	io.Reader
	hasher   hash.Hash
	expected string
	verified bool
}

func (r *hashVerifyingReader) Read(p []byte) (n int, err error) {
	n, err = r.Reader.Read(p)
	if n > 0 {
		r.hasher.Write(p[:n])
	}
	if err == io.EOF && !r.verified {
		actual := hex.EncodeToString(r.hasher.Sum(nil))
		if r.expected != "" && actual != r.expected {
			return n, fmt.Errorf("hash mismatch: expected %s, got %s", r.expected, actual)
		}
		r.verified = true
	}
	return n, err
}

// FsChunkInit securely allocates an upload_id and stores session info
func FsChunkInit(c *gin.Context) {
	var req struct {
		Path         string `json:"path"`
		TotalChunks  int    `json:"total_chunks"`
		LastModified int64  `json:"last_modified"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}

	path, err := resolveUserPath(c, req.Path)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}

	if err := checkWritePermission(path); err != nil {
		common.ErrorStrResp(c, "no write permission", 403)
		return
	}

	uploadId := uuid.NewString()
	chunkDir := getChunkDir(uploadId)
	if err := os.MkdirAll(chunkDir, 0755); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}

	sessionData := map[string]interface{}{
		"user_id":       getUserFromContext(c).ID,
		"path":          req.Path,
		"total_chunks":  req.TotalChunks,
		"last_modified": req.LastModified,
		"created_at":    time.Now().Unix(),
	}

	sessionBytes, _ := json.Marshal(sessionData)
	err = os.WriteFile(stdpath.Join(chunkDir, "session.json"), sessionBytes, 0644)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}

	common.SuccessResp(c, gin.H{
		"upload_id": uploadId,
	})
}

func getChunkDir(uploadId string) string {
	return stdpath.Join(conf.Conf.TempDir, "chunks", uploadId)
}

func getAndVerifyChunkSession(c *gin.Context, uploadId string) (map[string]interface{}, string, error) {
	chunkDir := getChunkDir(uploadId)
	sessionPath := stdpath.Join(chunkDir, "session.json")

	sessionBytes, err := os.ReadFile(sessionPath)
	if err != nil {
		return nil, "", fmt.Errorf("invalid upload_id or session expired")
	}

	var sessionData map[string]interface{}
	json.Unmarshal(sessionBytes, &sessionData)

	user := getUserFromContext(c)
	if float64(user.ID) != sessionData["user_id"].(float64) {
		return nil, "", fmt.Errorf("unauthorized access to chunk session")
	}

	return sessionData, chunkDir, nil
}

// FsChunkUpload handles uploading a single chunk of a large file
func FsChunkUpload(c *gin.Context) {
	uploadId := c.Query("upload_id")
	indexStr := c.Query("index")
	if uploadId == "" || indexStr == "" {
		common.ErrorStrResp(c, "upload_id and index are required", 400)
		return
	}

	if _, err := strconv.Atoi(indexStr); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}

	_, chunkDir, err := getAndVerifyChunkSession(c, uploadId)
	if err != nil {
		common.ErrorStrResp(c, err.Error(), 403)
		return
	}

	file, err := c.FormFile("file")
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}

	chunkPath := stdpath.Join(chunkDir, indexStr)
	expectedCRC32 := c.GetHeader("X-Chunk-CRC32")

	if err := c.SaveUploadedFile(file, chunkPath); err != nil {
		common.ErrorResp(c, err, 500)
		return
	}

	f, err := os.Open(chunkPath)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	defer f.Close()

	hasher := crc32.NewIEEE()
	io.Copy(hasher, f)
	actualCRC32 := hex.EncodeToString(hasher.Sum(nil))

	if expectedCRC32 != "" && actualCRC32 != expectedCRC32 {
		f.Close()
		os.Remove(chunkPath)
		common.ErrorStrResp(c, fmt.Sprintf("chunk CRC32 mismatch: client=%s, server=%s", expectedCRC32, actualCRC32), 400)
		return
	}

	common.SuccessResp(c, gin.H{
		"crc32": actualCRC32,
	})
}

// buildMergeStream prepares an io.MultiReader to stream all chunks without copying
func buildMergeStream(chunkDir, name string, totalChunks int, lastModified time.Time, expectedHash string) (*stream.FileStream, error) {
	var readers []io.Reader
	var closers []io.Closer
	var totalSize int64

	for i := 0; i < totalChunks; i++ {
		chunkPath := stdpath.Join(chunkDir, strconv.Itoa(i))
		f, err := os.Open(chunkPath)
		if err != nil {
			for _, c := range closers {
				c.Close()
			}
			return nil, fmt.Errorf("chunk %d not found or unreadable: %w", i, err)
		}
		stat, _ := f.Stat()
		totalSize += stat.Size()

		readers = append(readers, f)
		closers = append(closers, f)
	}

	multiReader := io.MultiReader(readers...)

	hasher := xxhash.New()
	verifyingReader := &hashVerifyingReader{
		Reader:   multiReader,
		hasher:   hasher,
		expected: expectedHash,
	}

	s := &stream.FileStream{
		Obj: &model.Object{
			Name:     name,
			Size:     totalSize,
			Modified: lastModified,
		},
		Reader:   verifyingReader,
		Mimetype: utils.GetMimeType(name),
	}

	s.Closers.Add(utils.CloseFunc(func() error {
		for _, c := range closers {
			c.Close()
		}
		os.RemoveAll(chunkDir)
		return nil
	}))

	return s, nil
}

// FsChunkMerge streams all chunks into a single file directly to storage
func FsChunkMerge(c *gin.Context) {
	var req struct {
		UploadId  string `json:"upload_id"`
		AsTask    bool   `json:"as_task"`
		Overwrite bool   `json:"overwrite"`
		Hash      string `json:"hash"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		common.ErrorResp(c, err, 400)
		return
	}

	sessionData, chunkDir, err := getAndVerifyChunkSession(c, req.UploadId)
	if err != nil {
		common.ErrorStrResp(c, err.Error(), 403)
		return
	}

	reqPath := sessionData["path"].(string)
	path, err := resolveUserPath(c, reqPath)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}

	if err := checkFileExists(c.Request.Context(), path, req.Overwrite); err != nil {
		common.ErrorStrResp(c, err.Error(), 403)
		return
	}

	totalChunks := int(sessionData["total_chunks"].(float64))

	for i := 0; i < totalChunks; i++ {
		chunkPath := stdpath.Join(chunkDir, strconv.Itoa(i))
		if _, err := os.Stat(chunkPath); os.IsNotExist(err) {
			common.ErrorStrResp(c, "chunk "+strconv.Itoa(i)+" not found", 400)
			return
		}
	}

	dir, name := stdpath.Split(path)
	if err := validateFileName(name); err != nil {
		os.RemoveAll(chunkDir)
		common.ErrorStrResp(c, err.Error(), 403)
		return
	}

	lastModified := time.Now()
	if lm, ok := sessionData["last_modified"].(float64); ok && lm > 0 {
		lastModified = time.UnixMilli(int64(lm))
	}

	s, err := buildMergeStream(chunkDir, name, totalChunks, lastModified, req.Hash)
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}

	s.WebPutAsTask = req.AsTask

	var t task.TaskExtensionInfo
	if req.AsTask {
		t, err = fs.PutAsTask(c.Request.Context(), dir, s)
	} else {
		err = fs.PutDirectly(c.Request.Context(), dir, s)
	}

	if err != nil {
		s.Closers.Close()
		common.ErrorResp(c, err, 500)
		return
	}

	if t == nil {
		common.SuccessResp(c)
		return
	}

	common.SuccessResp(c, gin.H{
		"task": getTaskInfo(t),
	})
}
