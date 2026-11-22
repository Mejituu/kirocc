package customprov

import (
	"encoding/json/v2"
	"net/http"
	"strings"
)

// unsupportedMediaPhrases are the modality-rejection messages the passive
// retry treats as an unsupported-image signal. Matching is case-insensitive
// across every JSON string value plus the raw body.
var unsupportedMediaPhrases = []string{
	"only supports text",
	"only support text",
	"does not support image",
	"not support image",
	"unexpected item type in content",
	"input content must be a string",
}

// mediaErrorExclusions mark errors that mention images but are not modality
// rejections (dimension limits, upload failures). They win over the positive
// phrases.
var mediaErrorExclusions = []string{
	"must be larger than",
	"image length and width",
	"failed to upload image",
}

// isUnsupportedMediaStatus reports whether status is one the passive retry
// inspects. The body still has to match isUnsupportedMediaError.
func isUnsupportedMediaStatus(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusUnsupportedMediaType,
		http.StatusUnprocessableEntity, http.StatusNotImplemented:
		return true
	default:
		return false
	}
}

// isUnsupportedMediaError reports whether an upstream error status/body
// matches the modality-rejection cases the passive retry handles.
func isUnsupportedMediaError(status int, body []byte) bool {
	if !isUnsupportedMediaStatus(status) {
		return false
	}

	// Decoding first recovers \u-escaped text (Chinese, above all) that the
	// raw body would not reveal as plain substrings. The raw body still
	// participates so non-JSON error strings match too.
	var values []string
	var decoded any
	if err := json.Unmarshal(body, &decoded); err == nil {
		values = collectJSONStrings(decoded, values)
	}
	values = append(values, string(body))
	text := strings.ToLower(strings.Join(values, "\n"))

	for _, phrase := range mediaErrorExclusions {
		if strings.Contains(text, phrase) {
			return false
		}
	}
	for _, phrase := range unsupportedMediaPhrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	// GLM rejects non-text content as "messages.content.type 参数非法，
	// 取值范围 ['text']".
	return strings.Contains(text, "content.type") && strings.Contains(text, "['text']")
}

// collectJSONStrings appends every string value reachable from v to dst.
func collectJSONStrings(v any, dst []string) []string {
	switch v := v.(type) {
	case string:
		dst = append(dst, v)
	case []any:
		for _, item := range v {
			dst = collectJSONStrings(item, dst)
		}
	case map[string]any:
		for _, item := range v {
			dst = collectJSONStrings(item, dst)
		}
	}
	return dst
}
