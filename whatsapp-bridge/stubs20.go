package main

// TODO(#20) stubs so the tests compile.

type HistoryResponse struct {
	Success         bool   `json:"success"`
	Message         string `json:"message"`
	RequestID       string `json:"request_id,omitempty"`
	OldestMessageID string `json:"oldest_message_id,omitempty"`
	Count           int    `json:"count,omitempty"`
}
