package asyncjob

import (
	"time"

	"github.com/bytedance/sonic"
	"github.com/maximhq/bifrost/core/schemas"
)

// Job is an asynchronous inference job persisted in async_jobs.
type Job struct {
	ID           string                 `gorm:"primaryKey;type:varchar(255)" json:"id"`
	Status       schemas.AsyncJobStatus `gorm:"type:varchar(50);index:idx_async_jobs_status;not null" json:"status"`
	RequestType  schemas.RequestType    `gorm:"type:varchar(50);index:idx_async_jobs_request_type;not null" json:"request_type"`
	Response     string                 `gorm:"type:text" json:"response"`
	StatusCode   int                    `gorm:"default:0" json:"status_code,omitempty"`
	Error        string                 `gorm:"type:text" json:"error,omitempty"`
	VirtualKeyID *string                `gorm:"type:varchar(255);index:idx_async_jobs_vk_id" json:"virtual_key_id,omitempty"`
	ResultTTL    int                    `gorm:"default:3600" json:"-"`
	ExpiresAt    *time.Time             `gorm:"index:idx_async_jobs_expires_at" json:"expires_at,omitempty"`
	CreatedAt    time.Time              `gorm:"index;not null" json:"created_at"`
	CompletedAt  *time.Time             `json:"completed_at,omitempty"`
}

func (Job) TableName() string {
	return "async_jobs"
}

// ToResponse converts a job row to the HTTP AsyncJobResponse shape.
func (j *Job) ToResponse() *schemas.AsyncJobResponse {
	resp := &schemas.AsyncJobResponse{
		ID:          j.ID,
		Status:      j.Status,
		ExpiresAt:   j.ExpiresAt,
		CreatedAt:   j.CreatedAt,
		CompletedAt: j.CompletedAt,
		StatusCode:  j.StatusCode,
	}

	if j.Response != "" {
		switch j.RequestType {
		case schemas.ResponsesRequest, schemas.ResponsesStreamRequest:
			var result schemas.BifrostResponsesResponse
			if err := sonic.Unmarshal([]byte(j.Response), &result); err == nil {
				resp.Result = &result
			}
		case schemas.ChatCompletionRequest, schemas.ChatCompletionStreamRequest:
			var result schemas.BifrostChatResponse
			if err := sonic.Unmarshal([]byte(j.Response), &result); err == nil {
				resp.Result = &result
			}
		case schemas.TextCompletionRequest, schemas.TextCompletionStreamRequest:
			var result schemas.BifrostTextCompletionResponse
			if err := sonic.Unmarshal([]byte(j.Response), &result); err == nil {
				resp.Result = &result
			}
		case schemas.EmbeddingRequest:
			var result schemas.BifrostEmbeddingResponse
			if err := sonic.Unmarshal([]byte(j.Response), &result); err == nil {
				resp.Result = &result
			}
		case schemas.SpeechRequest, schemas.SpeechStreamRequest:
			var result schemas.BifrostSpeechResponse
			if err := sonic.Unmarshal([]byte(j.Response), &result); err == nil {
				resp.Result = &result
			}
		case schemas.TranscriptionRequest, schemas.TranscriptionStreamRequest:
			var result schemas.BifrostTranscriptionResponse
			if err := sonic.Unmarshal([]byte(j.Response), &result); err == nil {
				resp.Result = &result
			}
		case schemas.ImageGenerationRequest, schemas.ImageGenerationStreamRequest,
			schemas.ImageEditRequest, schemas.ImageEditStreamRequest,
			schemas.ImageVariationRequest:
			var result schemas.BifrostImageGenerationResponse
			if err := sonic.Unmarshal([]byte(j.Response), &result); err == nil {
				resp.Result = &result
			}
		case schemas.CountTokensRequest:
			var result schemas.BifrostCountTokensResponse
			if err := sonic.Unmarshal([]byte(j.Response), &result); err == nil {
				resp.Result = &result
			}
		default:
			var result interface{}
			if err := sonic.Unmarshal([]byte(j.Response), &result); err == nil {
				resp.Result = result
			}
		}
		if resp.Result == nil {
			var raw interface{}
			if err := sonic.Unmarshal([]byte(j.Response), &raw); err == nil {
				resp.Result = raw
			}
		}
	}

	if j.Error != "" {
		var bifrostErr schemas.BifrostError
		if err := sonic.Unmarshal([]byte(j.Error), &bifrostErr); err == nil {
			resp.Error = &bifrostErr
		}
	}

	return resp
}
