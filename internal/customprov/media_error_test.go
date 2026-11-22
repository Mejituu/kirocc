package customprov

import (
	"net/http"
	"testing"
)

func TestIsUnsupportedMediaError(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		// Real samples from /tmp/kirocc-models/images.tsv.
		{
			name:   "anthropic glm-5.3 plain text modality rejection",
			status: http.StatusBadRequest,
			body:   "InvalidParameter | The current model only supports text modality and does not support image input. Please remove the image content from your messages and retry.",
			want:   true,
		},
		{
			name:   "anthropic glm-5.3 json-wrapped modality rejection",
			status: http.StatusBadRequest,
			body:   `{"type":"error","error":{"type":"invalid_request_error","code":"InvalidParameter","message":"The current model only supports text modality and does not support image input. Please remove the image content from your messages and retry."}}`,
			want:   true,
		},
		{
			name:   "openai glm-5.3 json with unicode escapes",
			status: http.StatusBadRequest,
			body:   `{"error":{"code":"invalid_parameter_error","message":"The current model only supports text modality and does not support image input."}}`,
			want:   true,
		},
		{
			name:   "openai zhipu glm-5.3 chinese content type rejection",
			status: http.StatusBadRequest,
			body:   `{"error":{"code":"1210","message":"messages.content.type 参数非法，取值范围 ['text']"}}`,
			want:   true,
		},
		{
			name:   "openai zhipu glm-5.3 chinese content type rejection with unicode escapes",
			status: http.StatusBadRequest,
			body:   `{"error":{"code":"1210","message":"messages.content.type 参数非法，取值范围 ['text']"}}`,
			want:   true,
		},
		{
			name:   "kimi upload failure is excluded",
			status: http.StatusBadRequest,
			body:   `UploadFailed | {"error":{"code":"UPLOAD_FAILED","message":"Failed to upload image"}}`,
			want:   false,
		},
		{
			name:   "qwen image dimension limit is excluded",
			status: http.StatusBadRequest,
			body:   "InvalidParameter | The image length and width do not meet the model restrictions. [height:1 or width:1 must be larger than 10]",
			want:   false,
		},

		// Positive phrases.
		{
			name:   "only support text singular",
			status: http.StatusBadRequest,
			body:   `{"error":{"message":"This model ONLY SUPPORT TEXT input"}}`,
			want:   true,
		},
		{
			name:   "only supports text on unprocessable entity",
			status: http.StatusUnprocessableEntity,
			body:   `{"error":{"message":"model only supports text"}}`,
			want:   true,
		},
		{
			name:   "unexpected item type in content nested",
			status: http.StatusBadRequest,
			body:   `{"errors":[{"details":{"message":"unexpected item type in content"}}]}`,
			want:   true,
		},
		{
			name:   "input content must be a string",
			status: http.StatusNotImplemented,
			body:   `{"error":{"message":"input content must be a string"}}`,
			want:   true,
		},
		{
			name:   "non-json plain text phrase",
			status: http.StatusBadRequest,
			body:   "bad request: input content must be a string",
			want:   true,
		},

		// Negative cases.
		{
			name:   "bare 415 unsupported media type",
			status: http.StatusUnsupportedMediaType,
			body:   "Unsupported Media Type",
			want:   false,
		},
		{
			name:   "maximum context length",
			status: http.StatusBadRequest,
			body:   `{"error":{"message":"This model's maximum context length is 128000 tokens"}}`,
			want:   false,
		},
		{
			name:   "positive phrase on rate limit status",
			status: http.StatusTooManyRequests,
			body:   `{"error":{"message":"only supports text"}}`,
			want:   false,
		},
		{
			name:   "positive phrase on not found status",
			status: http.StatusNotFound,
			body:   `{"error":{"message":"only supports text"}}`,
			want:   false,
		},
		{
			name:   "empty body",
			status: http.StatusBadRequest,
			body:   "",
			want:   false,
		},

		// Exclusion wins over a positive phrase.
		{
			name:   "exclusion wins over modality phrase",
			status: http.StatusBadRequest,
			body:   `{"error":{"message":"The model does not support image input; height must be larger than 10"}}`,
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isUnsupportedMediaError(tt.status, []byte(tt.body)); got != tt.want {
				t.Errorf("isUnsupportedMediaError(%d, %q) = %v, want %v", tt.status, tt.body, got, tt.want)
			}
		})
	}
}
