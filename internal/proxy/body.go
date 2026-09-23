package proxy

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// BodyPolicy 决定畸形（非对象 / 类型错误的 JSON）请求体的处理方式。
//
// 背景（proxy_handler.py:212、proxy_support.py:54）：参照实现把**任何**解析失败
// 或非对象的 body 静默变成 `{}`，于是请求继续往下走，最终在 `_resolve_model_id`
// 处因为缺少 model 而返回 400「请求体中缺少 model 字段」。这个文案对「body 是
// `[1,2,3]`」或「body 是 `{"model":}`」都不成立，调用方会拿着一个错误的诊断去
// 查自己的请求。
//
// 产品决策是改为显式 400，但**保留参照行为**作为可选档，因为它是既定契约
// （迁移期要能与参照实现逐字节比对，该档就是比对用的）。
type BodyPolicy string

const (
	// BodyPolicyStrict 对畸形 body 返回显式 400 + 可诊断消息（默认，产品决策）。
	BodyPolicyStrict BodyPolicy = "strict"
	// BodyPolicyPython 复刻参照实现：静默变 {} ⇒ 400「缺少 model 字段」。
	BodyPolicyPython BodyPolicy = "python"
)

// MultipartPolicy 决定 multipart/form-data 的处理方式。
//
// 背景（proxy_support.py:279-301 与迁移方案 §4.5）：参照实现把
// `/v1/images/edits` 的请求体整体当 JSON 解析。multipart 当然解析不出对象，于是
// payload 变成 `{}`，`_resolve_model_id` 拿不到 model，请求直接 400「请求体中缺少
// model 字段」——也就是说 **multipart 的图片编辑请求在参照实现下从来没能成功过**
// （现有测试发的都是 JSON）。产品决策要求把它修好。
type MultipartPolicy string

const (
	// MultipartReject 显式 415 拒绝，绝不静默改写（默认）。
	//
	// 之所以把「拒绝」设为默认而不是「支持」：本包只做编排，从表单里取出 model
	// 用于路由**可以**做到（见 MultipartPassthrough），但把「image 字段是文件还是
	// 下载 URL」「mask 是否必需」这类语义做对需要图片专用的适配层，不在本包范围内。
	// 一个明确的 415 比一个「看起来转发成功但上游收到空 JSON」好得多——后者正是
	// 参照实现的现状。
	MultipartReject MultipartPolicy = "reject"
	// MultipartPassthrough 缓冲表单、从表单字段里取 model 用于路由，然后把
	// **原始字节**与原始 Content-Type 一并转发给上游。
	//
	// 这是本包能给出的完整实现：路由只需要 model，其余字段（image、prompt、mask…）
	// 对 AMKR 是不透明的，原样转发即可。
	MultipartPassthrough MultipartPolicy = "passthrough"
	// MultipartPython 复刻参照实现：multipart 字节被当 JSON 解析 ⇒ payload={} ⇒
	// 400「缺少 model 字段」。保留它是为了迁移期能与参照实现逐字节比对。
	MultipartPython MultipartPolicy = "python"
)

// bodyError 是请求体校验失败，可直接折算成下游响应。
type bodyError struct {
	statusCode int
	message    string
}

func (e *bodyError) Error() string { return e.message }

// malformedBodyError 表示请求体不是合法 JSON。
//
// 文案刻意与「缺少 model 字段」区分：后者是参照实现在这类输入上的既有文案，但
// 它对畸形 body 并不成立，会误导排查方向。
func malformedBodyError() *bodyError {
	return &bodyError{statusCode: http.StatusBadRequest, message: "请求体不是合法的 JSON"}
}

// nonObjectBodyError 表示请求体是合法 JSON 但不是对象。
func nonObjectBodyError(kind string) *bodyError {
	return &bodyError{
		statusCode: http.StatusBadRequest,
		message:    "请求体必须是 JSON 对象，实际是 " + kind,
	}
}

// unsupportedMultipartError 表示本实现拒绝 multipart/form-data。
func unsupportedMultipartError() *bodyError {
	return &bodyError{
		statusCode: http.StatusUnsupportedMediaType,
		message: "multipart/form-data 请求体不被支持：" +
			"AMKR 需要 JSON 请求体才能解析 model。请改用 JSON 形式的 /v1/images/edits。",
	}
}

// bodyTooLargeError 表示请求体超过缓冲上限。
func bodyTooLargeError(limit int64) *bodyError {
	return &bodyError{
		statusCode: http.StatusRequestEntityTooLarge,
		message:    "请求体超过上限 " + strconv.FormatInt(limit, 10) + " 字节",
	}
}

// readRequestBody 读取并校验请求体。
//
// 返回 (payload, rawBody, flat, error)：
//   - payload 是解析出的 JSON 对象；multipart 或畸形 body 时是 `{}` 形状的占位；
//   - rawBody 是**原始字节**，转发给上游时用它（字节级透传是契约）；
//   - flat 表示「body 不是 JSON 对象」（multipart 表单），调用方据此跳过 JSON 语义
//     的判断（如 stream 嗅探）；
//   - error 非 nil 时应当直接写回下游（决策 3：畸形体在 HTTP 边界被挡住）。
func (h *Handler) readRequestBody(request *http.Request, contentType string) (*canonical.Value, []byte, bool, error) {
	if isMultipartContentType(contentType) {
		return h.readMultipartBody(request, contentType)
	}
	body, err := readAllLimited(request.Body, h.maxJSONBytes)
	if err != nil {
		return nil, nil, false, err
	}
	payload, bodyErr := parseJSONObject(body, h.bodyPolicy)
	if bodyErr != nil {
		return nil, nil, false, bodyErr
	}
	return payload, body, false, nil
}

// parseJSONObject 解析请求体为 JSON 对象，按 policy 决定畸形输入的处理。
func parseJSONObject(body []byte, policy BodyPolicy) (*canonical.Value, error) {
	if len(body) == 0 {
		// 空体：参照实现走 `if not body: return {}`，于是 400「缺少 model 字段」。
		// 空体不是「畸形」，因此 strict 档下也不额外报错——保持参照文案。
		return canonical.NewObject(), nil
	}
	value, err := canonical.Parse(body)
	if err != nil {
		if policy == BodyPolicyPython {
			// 复刻参照实现档：静默变 {}，后续自然 400「缺少 model 字段」。
			return canonical.NewObject(), nil
		}
		return nil, malformedBodyError()
	}
	if !value.IsObject() {
		if policy == BodyPolicyPython {
			return canonical.NewObject(), nil
		}
		return nil, nonObjectBodyError(canonical.PyTypeName(value))
	}
	return value, nil
}

// readMultipartBody 按 MultipartPolicy 处理 multipart/form-data 请求体。
func (h *Handler) readMultipartBody(request *http.Request, contentType string) (*canonical.Value, []byte, bool, error) {
	body, err := readAllLimited(request.Body, h.maxMultipart)
	if err != nil {
		return nil, nil, false, err
	}
	switch h.multipart {
	case MultipartReject:
		return nil, nil, false, unsupportedMultipartError()
	case MultipartPython:
		// 复刻参照实现：payload 变 {}，rawBody 保留原始字节（虽然没人会用到它）。
		return canonical.NewObject(), body, true, nil
	}
	model, mediaError := multipartModelField(contentType, body)
	if mediaError != nil {
		return nil, nil, false, mediaError
	}
	payload := canonical.NewObject()
	if model != "" {
		payload.Obj.Set("model", canonical.NewString(model))
	}
	return payload, body, true, nil
}

// multipartModelField 从表单里取出 model 字段。
//
// 只读 model 且只读前 4 KiB：其余字段（图片内容）对 AMKR 不透明，原样转发；
// 用 multipart.Reader 逐 part 迭代而不是 ReadForm，是为了避免把图片内容解析进
// 内存（ReadForm 会把文件部分缓冲到磁盘）。
func multipartModelField(contentType string, body []byte) (string, error) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", malformedBodyError()
	}
	boundary := params["boundary"]
	if boundary == "" {
		return "", malformedBodyError()
	}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, nextErr := reader.NextPart()
		if nextErr != nil {
			if errors.Is(nextErr, io.EOF) {
				// 没有 model 字段：交给后续的「缺少 model 字段」分支处理。
				return "", nil
			}
			return "", malformedBodyError()
		}
		if part.FormName() != "model" {
			_ = part.Close()
			continue
		}
		value, readErr := readAllLimited(part, 4096)
		_ = part.Close()
		if readErr != nil {
			return "", readErr
		}
		return strings.TrimSpace(string(value)), nil
	}
}

// isMultipartContentType 判断是否为 multipart/form-data。
//
// 必须经 mime.ParseMediaType 规整：`multipart/form-data; boundary=...` 是常规形态，
// 而盲目 strings.HasPrefix 会被 `multipart/form-datax` 骗过去。
func isMultipartContentType(contentType string) bool {
	if contentType == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return strings.EqualFold(mediaType, "multipart/form-data")
}

// readAllLimited 读 reader，超过 limit（<=0 表示不限）时报错。
//
// 用 limit+1 读再判断，是为了区分「恰好等于上限」与「超过上限」——只读 limit 个
// 字节无法分辨这两种情况。
func readAllLimited(reader io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return io.ReadAll(reader)
	}
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, bodyTooLargeError(limit)
	}
	return body, nil
}
