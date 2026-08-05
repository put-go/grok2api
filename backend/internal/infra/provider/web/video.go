package web

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

type webMediaUpstreamError struct {
	status              int
	summary             string
	antiBotRejected     bool
	bodyBytes           int
	bodyTruncated       bool
	bodyPrefixSHA256    string
	bodyKind            string
	cloudflareChallenge bool
}

type webVideoIncompleteError struct {
	lastProgress   int
	completed      bool
	videoIDPresent bool
	userIDPresent  bool
}

func (e *webVideoIncompleteError) Error() string {
	if e == nil {
		return ""
	}
	if e.completed {
		return fmt.Sprintf(
			"视频生成已完成但响应没有可用内容 URL（video_id=%t, user_id=%t）",
			e.videoIDPresent,
			e.userIDPresent,
		)
	}
	return fmt.Sprintf(
		"视频生成响应提前结束（last_progress=%d, video_id=%t）",
		e.lastProgress,
		e.videoIDPresent,
	)
}

func (e *webVideoIncompleteError) Unwrap() error {
	return provider.ErrUpstreamStreamIncomplete
}

func (*webVideoIncompleteError) HTTPStatusCode() int {
	return http.StatusBadGateway
}

func (e *webMediaUpstreamError) Error() string {
	if e == nil {
		return ""
	}
	return e.summary
}

func (e *webMediaUpstreamError) HTTPStatusCode() int {
	if e == nil {
		return 0
	}
	return e.status
}

func (e *webMediaUpstreamError) Unwrap() error {
	if e != nil && e.antiBotRejected {
		return provider.ErrAntiBotRejected
	}
	return nil
}

const (
	webMediaDiagnosticBodyLimit    = 64 << 10
	webMediaDiagnosticSummaryLimit = 256
	webMediaDiagnosticFieldLimit   = 160
)

var (
	webMediaAuthorizationPattern = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]+`)
	webMediaCookiePattern        = regexp.MustCompile(`(?i)\b(cookie|set-cookie)\b\s*[:=]\s*[^\r\n]+`)
	webMediaSecretPattern        = regexp.MustCompile(`(?i)(["']?(?:authorization|proxy-authorization|x-api-key|api[_-]?key|access[_-]?token|refresh[_-]?token|id[_-]?token|upload[_-]?url|cookie|sso|session[_-]?id)["']?\s*[:=]\s*["']?)[^"'\s,;}]+`)
	webMediaJWTPattern           = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{12,}\.[A-Za-z0-9_-]{12,}(?:\.[A-Za-z0-9_-]{12,})?\b`)
	webMediaEmailPattern         = regexp.MustCompile(`(?i)\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b`)
	webMediaURLPattern           = regexp.MustCompile(`https?://[^\s"'<>]+`)
	webMediaLongTokenPattern     = regexp.MustCompile(`[A-Za-z0-9+/=_-]{256,}`)
)

// newWebMediaUpstreamError keeps the HTTP status while exposing only a
// bounded, redacted summary through the error. Structured logs retain only
// body metadata and a prefix hash, never the upstream response body itself.
func newWebMediaUpstreamError(status int, body []byte, truncated bool) *webMediaUpstreamError {
	digest := sha256.Sum256(body)
	return &webMediaUpstreamError{
		status:              status,
		summary:             summarizeWebMediaUpstreamError(status, body, truncated),
		antiBotRejected:     isWebMediaAntiBotRejection(status, body),
		bodyBytes:           len(body),
		bodyTruncated:       truncated,
		bodyPrefixSHA256:    fmt.Sprintf("%x", digest),
		bodyKind:            classifyWebMediaDiagnosticBody(body),
		cloudflareChallenge: isCloudflareChallengeBody(body),
	}
}

func isWebMediaAntiBotRejection(status int, body []byte) bool {
	if status != http.StatusForbidden {
		return false
	}
	code, message, structured := extractWebMediaUpstreamErrorFields(body)
	return structured && (code == "7" || strings.Contains(strings.ToLower(message), "anti-bot"))
}

func classifyWebMediaDiagnosticBody(body []byte) string {
	if !utf8.Valid(body) {
		return "binary"
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return "empty"
	}
	if json.Valid(body) {
		return "json"
	}
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<html") {
		return "html"
	}
	for _, value := range trimmed {
		if value < 0x20 && value != '\t' && value != '\r' && value != '\n' {
			return "binary"
		}
	}
	return "text"
}

func isCloudflareChallengeBody(body []byte) bool {
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "just a moment") ||
		strings.Contains(lower, "challenge-platform") ||
		strings.Contains(lower, "__cf_chl") ||
		strings.Contains(lower, "cf-chl-")
}

func (a *Adapter) logWebMediaUpstreamRejection(stage string, response *http.Response, upstreamErr *webMediaUpstreamError) {
	if upstreamErr == nil {
		return
	}
	attributes := []any{
		"stage", stage,
		"status", upstreamErr.status,
		"body_bytes_captured", upstreamErr.bodyBytes,
		"body_truncated", upstreamErr.bodyTruncated,
		"body_prefix_sha256", upstreamErr.bodyPrefixSHA256,
		"body_kind", upstreamErr.bodyKind,
		"cloudflare_challenge", upstreamErr.cloudflareChallenge,
	}
	if response != nil {
		attributes = append(attributes,
			"content_type", safeWebMediaDiagnostic(response.Header.Get("Content-Type"), 128),
			"content_length", response.ContentLength,
			"content_encoding", safeWebMediaDiagnostic(response.Header.Get("Content-Encoding"), 64),
			"server", safeWebMediaDiagnostic(response.Header.Get("Server"), 128),
			"cf_ray", safeWebMediaDiagnostic(response.Header.Get("CF-Ray"), 128),
			"upstream_request_id", safeWebMediaDiagnostic(firstNonEmpty(response.Header.Get("X-Request-Id"), response.Header.Get("X-Xai-Request-Id")), 128),
		)
	}
	a.log().Warn("web_media_upstream_rejected", attributes...)
}

func summarizeWebMediaUpstreamError(status int, body []byte, truncated bool) string {
	code, message, structured := extractWebMediaUpstreamErrorFields(body)
	parts := []string{fmt.Sprintf("Grok Web 媒体上游返回 %d", status)}
	if code != "" {
		parts = append(parts, code)
	}
	if message != "" {
		parts = append(parts, message)
	} else if len(strings.TrimSpace(string(body))) == 0 {
		parts = append(parts, "<empty>")
	} else if truncated {
		parts = append(parts, "响应正文过长")
	} else if !structured {
		parts = append(parts, "响应正文不可解析")
	} else if code == "" {
		parts = append(parts, "未提供错误详情")
	}
	return boundWebMediaDiagnostic(strings.Join(parts, ": "), webMediaDiagnosticSummaryLimit)
}

func extractWebMediaUpstreamErrorFields(body []byte) (code, message string, structured bool) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return "", "", false
	}
	structured = true
	if errorObject, ok := root["error"].(map[string]any); ok {
		code = firstWebMediaDiagnosticCode(errorObject, "code", "type", "error")
		message = firstString(errorObject, "message", "error", "detail")
	} else if errorText, ok := root["error"].(string); ok {
		message = errorText
	}
	if code == "" {
		code = firstWebMediaDiagnosticCode(root, "code", "error_code", "type")
	}
	if message == "" {
		message = firstString(root, "message", "error_message", "detail")
	}
	return safeWebMediaDiagnostic(code, 64), safeWebMediaDiagnostic(message, webMediaDiagnosticFieldLimit), true
}

func firstWebMediaDiagnosticCode(value map[string]any, keys ...string) string {
	if code := firstString(value, keys...); code != "" {
		return code
	}
	if code, ok := firstInt(value, keys...); ok {
		return fmt.Sprintf("%d", code)
	}
	return ""
}

func safeWebMediaDiagnostic(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	value = webMediaCookiePattern.ReplaceAllString(value, "$1: [REDACTED]")
	value = webMediaAuthorizationPattern.ReplaceAllString(value, "$1 [REDACTED]")
	value = webMediaSecretPattern.ReplaceAllString(value, "$1[REDACTED]")
	value = webMediaJWTPattern.ReplaceAllString(value, "[REDACTED]")
	value = webMediaEmailPattern.ReplaceAllString(value, "[REDACTED_EMAIL]")
	value = webMediaURLPattern.ReplaceAllString(value, "[REDACTED_URL]")
	value = webMediaLongTokenPattern.ReplaceAllString(value, "[REDACTED_LONG_VALUE]")
	return boundWebMediaDiagnostic(value, limit)
}

func boundWebMediaDiagnostic(value string, limit int) string {
	if limit <= 0 || value == "" {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func (a *Adapter) GenerateVideo(ctx context.Context, request provider.VideoRequest) (provider.VideoResult, error) {
	if len(request.ReferenceURLs) > mediadomain.MaxReferenceImages {
		return provider.VideoResult{}, fmt.Errorf("视频参考图片不能超过 %d 张", mediadomain.MaxReferenceImages)
	}
	if len(request.ReferenceURLs) > 1 && request.Duration > mediadomain.MaxReferenceVideoDuration {
		return provider.VideoResult{}, fmt.Errorf("多参考图视频的 duration 不能超过 %d 秒", mediadomain.MaxReferenceVideoDuration)
	}
	cfg := a.config()
	token, err := a.cipher.Decrypt(request.Credential.EncryptedAccessToken)
	if err != nil {
		return provider.VideoResult{}, err
	}
	lease, err := a.egress.AcquireCredential(ctx, domainegress.ScopeWeb, request.Credential)
	if err != nil {
		return provider.VideoResult{}, err
	}
	defer lease.Release()
	parentID := ""
	referenceAssetIDs := make([]string, 0, len(request.ReferenceURLs))
	for _, rawReference := range request.ReferenceURLs {
		assetID, referenceErr := a.prepareVideoReference(ctx, cfg, lease, token, rawReference)
		if referenceErr != nil {
			return provider.VideoResult{}, referenceErr
		}
		referenceAssetIDs = append(referenceAssetIDs, assetID)
	}
	if len(referenceAssetIDs) == 0 {
		parentID, err = a.createMediaPost(ctx, cfg, lease, token, "MEDIA_POST_TYPE_VIDEO", "", request.Prompt, "video_prompt_media_post")
		if err != nil {
			return provider.VideoResult{}, err
		}
	}
	segments := videoSegments(request.Duration)
	if len(segments) == 0 {
		return provider.VideoResult{}, fmt.Errorf("duration 必须在 1 到 15 秒之间")
	}
	ratio := resolveAspectRatio(request.AspectRatio)
	resolution := request.Resolution
	if resolution == "" {
		resolution = "720p"
	}
	payload := videoCreatePayload(request.Prompt, parentID, ratio, resolution, segments[0], referenceAssetIDs)
	response, err := a.postJSON(ctx, cfg, lease, token, cfg.BaseURL+"/rest/app-chat/conversations/new", payload, time.Duration(cfg.VideoTimeoutSeconds)*time.Second)
	if err != nil {
		return provider.VideoResult{}, err
	}
	outcome, parseErr := parseVideoStreamDetailed(response, request.Progress)
	_ = response.Body.Close()
	if parseErr != nil {
		if upstreamErr, ok := parseErr.(*webMediaUpstreamError); ok {
			a.logWebMediaUpstreamRejection("video_generation", response, upstreamErr)
		}
		return provider.VideoResult{}, parseErr
	}
	result, reconstructed, resultErr := outcome.finalResult(request.Credential.UserID)
	a.log().Info(
		"video_generation_stream_terminal",
		"job_id", request.JobID,
		"account_id", request.Credential.ID,
		"response_shape", outcome.responseShape,
		"last_progress", outcome.lastProgress,
		"completed", outcome.completed,
		"moderated", outcome.moderated,
		"video_id_present", outcome.videoID != "",
		"video_url_present", outcome.result.URL != "",
		"url_reconstructed", reconstructed,
	)
	if resultErr != nil {
		return provider.VideoResult{}, resultErr
	}
	if reconstructed {
		a.log().Warn(
			"video_generation_url_reconstructed",
			"job_id", request.JobID,
			"account_id", request.Credential.ID,
			"last_progress", outcome.lastProgress,
		)
	}
	return result, nil
}

func (a *Adapter) prepareVideoReference(ctx context.Context, cfg Config, lease *egress.Lease, token, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("视频参考图片 URL 不能为空")
	}
	image, err := a.loadChatImage(ctx, lease, value, 20<<20)
	if err != nil {
		return "", err
	}
	uploaded, err := a.uploadFileV2Direct(ctx, cfg, lease, token, image, cfg.BaseURL+"/imagine", imagineSelfUploadSource, "video_reference_upload")
	if err != nil {
		return "", err
	}
	if uploaded.ID == "" {
		return "", fmt.Errorf("上传视频参考图片后未返回 fileMetadataId")
	}
	return uploaded.ID, nil
}

type videoContentReadCloser struct {
	io.ReadCloser
	release func()
}

func (c *videoContentReadCloser) Close() error {
	err := c.ReadCloser.Close()
	if c.release != nil {
		c.release()
		c.release = nil
	}
	return err
}

// DownloadVideo retrieves a completed Grok asset through its source SSO
// session. Direct asset URLs are not public and must not be exposed as a
// substitute for this authenticated transfer.
func (a *Adapter) DownloadVideo(ctx context.Context, credential account.Credential, rawURL string) (io.ReadCloser, string, int64, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme != "https" || !trustedImageAssetHost(parsed.Hostname()) || parsed.User != nil {
		return nil, "", 0, fmt.Errorf("视频内容 URL 不受信任")
	}
	token, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return nil, "", 0, err
	}
	// 视频生成与成品下载必须复用同一账号身份；否则 Resin 会为 WebAsset
	// 重新分配租约，账号级 Cloudflare clearance 也不会进入下载请求。
	lease, err := a.egress.AcquireCredential(ctx, domainegress.ScopeWebAsset, credential)
	if err != nil {
		return nil, "", 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		lease.Release()
		return nil, "", 0, err
	}
	request.Header = buildHeaders(token, lease, "")
	request.Header.Del("Content-Type")
	response, err := lease.Do(request)
	if err != nil {
		lease.Release()
		return nil, "", 0, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_ = response.Body.Close()
		lease.Release()
		return nil, "", 0, fmt.Errorf("下载视频返回 %d", response.StatusCode)
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = "video/mp4"
	}
	if !strings.HasPrefix(contentType, "video/") {
		_ = response.Body.Close()
		lease.Release()
		return nil, "", 0, fmt.Errorf("上游视频 Content-Type 无效")
	}
	return &videoContentReadCloser{ReadCloser: response.Body, release: lease.Release}, contentType, response.ContentLength, nil
}

type videoStreamOutcome struct {
	result        provider.VideoResult
	videoID       string
	postID        string
	userID        string
	responseShape string
	lastProgress  int
	completed     bool
	moderated     bool
}

func (o videoStreamOutcome) finalResult(credentialUserID string) (provider.VideoResult, bool, error) {
	if o.moderated {
		return provider.VideoResult{}, false, provider.ErrContentPolicyViolation
	}
	if o.result.URL != "" {
		return o.result, false, nil
	}
	userID := strings.TrimSpace(o.userID)
	if userID == "" {
		userID = strings.TrimSpace(credentialUserID)
	}
	videoID := strings.TrimSpace(o.videoID)
	if o.completed && userID != "" && videoID != "" {
		return provider.VideoResult{
			URL:         generatedVideoAssetURL(userID, videoID),
			ContentType: "video/mp4",
		}, true, nil
	}
	return provider.VideoResult{}, false, &webVideoIncompleteError{
		lastProgress:   o.lastProgress,
		completed:      o.completed,
		videoIDPresent: videoID != "",
		userIDPresent:  userID != "",
	}
}

func generatedVideoAssetURL(userID, videoID string) string {
	return absoluteAssetURL("users/" + url.PathEscape(userID) + "/generated/" + url.PathEscape(videoID) + "/generated_video.mp4")
}

func videoUserIDFromAssetReference(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "https" || !trustedImageAssetHost(parsed.Hostname()) || parsed.User != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "users" || parts[1] == "_" {
		return ""
	}
	userID, err := url.PathUnescape(parts[1])
	if err != nil {
		return ""
	}
	return strings.TrimSpace(userID)
}

func parseVideoStream(response *http.Response, progress func(int)) (provider.VideoResult, string, error) {
	outcome, err := parseVideoStreamDetailed(response, progress)
	if err != nil {
		return provider.VideoResult{}, "", err
	}
	return outcome.result, outcome.postID, nil
}

func parseVideoStreamDetailed(response *http.Response, progress func(int)) (videoStreamOutcome, error) {
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, webMediaDiagnosticBodyLimit+1))
		if response.StatusCode == http.StatusUnauthorized {
			return videoStreamOutcome{}, provider.ErrUnauthorized
		}
		truncated := len(body) > webMediaDiagnosticBodyLimit
		if truncated {
			body = body[:webMediaDiagnosticBodyLimit]
		}
		return videoStreamOutcome{}, newWebMediaUpstreamError(response.StatusCode, body, truncated)
	}
	var outcome videoStreamOutcome
	handle := func(root map[string]any) (bool, error) {
		if errorValue, ok := root["error"].(map[string]any); ok {
			return false, webMediaStreamError(errorValue)
		}
		payload, responseShape := videoResponsePayload(root)
		if errorValue := nestedMap(payload, "error"); errorValue != nil {
			return false, webMediaStreamError(errorValue)
		}
		stream := nestedMap(payload, "streamingVideoGenerationResponse")
		if stream != nil {
			outcome.responseShape = responseShape
			if value, ok := numberAsInt(stream["progress"]); ok {
				outcome.lastProgress = max(outcome.lastProgress, value)
				outcome.completed = outcome.completed || value >= 100
				if progress != nil {
					progress(value)
				}
			}
			if value := firstString(stream, "assetId", "videoId", "videoPostId"); value != "" {
				outcome.videoID = value
			}
			if value := firstString(stream, "videoPostId", "videoId"); value != "" {
				outcome.postID = value
			}
			if value := videoUserIDFromAssetReference(firstString(stream, "imageReference")); value != "" {
				outcome.userID = value
			}
			moderated, _ := stream["moderated"].(bool)
			if moderated {
				outcome.moderated = true
				return false, nil
			}
			if setVideoResultURL(&outcome.result, firstString(stream, "videoUrl", "contentUrl", "contentURL", "assetUrl", "assetURL", "fileUri", "fileURL")) {
				outcome.completed = true
			}
		}
		attachments := videoFileAttachments(payload)
		if len(attachments) > 0 && outcome.responseShape == "" {
			outcome.responseShape = responseShape
		}
		for _, attachment := range attachments {
			if setVideoResultURL(&outcome.result, attachment) {
				outcome.completed = true
				continue
			}
			if !strings.ContainsAny(attachment, "/\\") {
				outcome.videoID = strings.TrimSpace(attachment)
				outcome.completed = outcome.videoID != ""
			}
		}
		return false, nil
	}

	reader := bufio.NewReader(response.Body)
	prefix, _ := reader.Peek(64)
	trimmedPrefix := strings.TrimSpace(string(prefix))
	var err error
	if strings.HasPrefix(trimmedPrefix, "data:") || strings.HasPrefix(trimmedPrefix, "event:") {
		err = consumeVideoSSE(reader, handle)
	} else {
		err = consumeVideoJSON(reader, handle)
	}
	if err != nil {
		return videoStreamOutcome{}, err
	}
	return outcome, nil
}

func videoResponsePayload(root map[string]any) (map[string]any, string) {
	result := nestedMap(root, "result")
	if result == nil {
		return root, "root"
	}
	if response := nestedMap(result, "response"); response != nil {
		return response, "result.response"
	}
	return result, "result"
}

func webMediaStreamError(value map[string]any) error {
	code := safeWebMediaDiagnostic(firstWebMediaDiagnosticCode(value, "code", "error_code", "type"), 64)
	message := safeWebMediaDiagnostic(firstString(value, "message", "error", "detail"), webMediaDiagnosticFieldLimit)
	switch {
	case code != "" && message != "":
		message = code + ": " + message
	case code != "":
		message = code
	case message == "":
		message = "未提供错误详情"
	}
	return fmt.Errorf("视频上游错误: %s", message)
}

func videoFileAttachments(payload map[string]any) []string {
	modelResponse := nestedMap(payload, "modelResponse")
	if modelResponse == nil {
		return nil
	}
	values, _ := modelResponse["fileAttachments"].([]any)
	attachments := make([]string, 0, len(values))
	for _, value := range values {
		if attachment, _ := value.(string); attachment != "" {
			attachments = append(attachments, attachment)
		}
	}
	return attachments
}

func setVideoResultURL(result *provider.VideoResult, value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	lower := strings.ToLower(value)
	if !strings.HasSuffix(strings.SplitN(lower, "?", 2)[0], ".mp4") && !strings.Contains(lower, "/content") {
		return false
	}
	result.URL = absoluteAssetURL(value)
	result.ContentType = "video/mp4"
	return true
}

func consumeVideoSSE(reader io.Reader, handle func(map[string]any) (bool, error)) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "data:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
		if line == "" || line == "[DONE]" || !strings.HasPrefix(line, "{") {
			continue
		}
		var root map[string]any
		if err := json.Unmarshal([]byte(line), &root); err != nil {
			return fmt.Errorf("%w: 解析视频上游 SSE: %v", provider.ErrUpstreamStreamIncomplete, err)
		}
		complete, err := handle(root)
		if err != nil {
			return err
		}
		if complete {
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%w: 读取视频上游流: %v", provider.ErrUpstreamStreamIncomplete, err)
	}
	return nil
}

func consumeVideoJSON(reader io.Reader, handle func(map[string]any) (bool, error)) error {
	decoder := json.NewDecoder(io.LimitReader(reader, 64<<20))
	for {
		var root map[string]any
		if err := decoder.Decode(&root); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("%w: 解析视频上游流: %v", provider.ErrUpstreamStreamIncomplete, err)
		}
		complete, err := handle(root)
		if err != nil {
			return err
		}
		if complete {
			return nil
		}
	}
}

func nestedMap(value map[string]any, keys ...string) map[string]any {
	current := value
	for _, key := range keys {
		next, ok := current[key].(map[string]any)
		if !ok {
			return nil
		}
		current = next
	}
	return current
}

func videoSegments(seconds int) []int {
	if seconds < 1 || seconds > mediadomain.MaxVideoDuration {
		return nil
	}
	return []int{seconds}
}

func videoCreatePayload(prompt, parentID, ratio, resolution string, seconds int, referenceAssetIDs []string) map[string]any {
	if len(referenceAssetIDs) > 0 {
		mode := "custom"
		inputKind := "referenceToVideo"
		input := map[string]any{
			"prompt": prompt, "inputAssets": referenceAssetIDs, "aspectRatio": ratio,
			"duration": seconds, "resolutionName": resolution,
		}
		if len(referenceAssetIDs) == 1 {
			inputKind = "imageToVideo"
			if strings.TrimSpace(prompt) == "" {
				mode = "normal"
			}
			input["mode"] = mode
		}
		return map[string]any{
			"modelName": "imagine-video-gen", "message": videoModeMessage(prompt, mode),
			"enableImageStreaming": true, "enableSideBySide": true, "sendFinalMetadata": true,
			"responseMetadata": map[string]any{
				"experiments": []any{}, "modelConfigOverride": map[string]any{"modelMap": map[string]any{}},
			},
			"mediaGenInput": map[string]any{inputKind: input},
			"kind":          "CONVERSATION_KIND_IMAGINE",
		}
	}
	config := map[string]any{"parentPostId": parentID, "aspectRatio": ratio, "videoLength": seconds, "resolutionName": resolution}
	return map[string]any{
		"temporary": true, "modelName": "imagine-video-gen", "message": prompt + " --mode=custom", "enableSideBySide": true,
		"responseMetadata": map[string]any{"experiments": []any{}, "modelConfigOverride": map[string]any{"modelMap": map[string]any{"videoGenModelConfig": config}}},
	}
}

func videoModeMessage(prompt, mode string) string {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "--mode=" + mode
	}
	return prompt + " --mode=" + mode
}
