package main

// TODO(#19) stubs so the tests compile.

import (
	"context"
	"database/sql"
	"errors"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waMmsRetry"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

type mediaFetcher interface {
	Download(ctx context.Context, msg whatsmeow.DownloadableMessage) ([]byte, error)
	SendMediaRetryReceipt(ctx context.Context, message *types.MessageInfo, mediaKey []byte) error
}

type mediaResult struct{ MediaType, Filename, OriginalFilename, Path string }

type mediaService struct {
	decrypt func(*events.MediaRetry, []byte) (*waMmsRetry.MediaRetryNotification, error)
}

var errMediaRetryRequested = errors.New("stub")

func newMediaService(f mediaFetcher, s *MessageStore) *mediaService { return &mediaService{} }
func (m *mediaService) download(id, chat string) (mediaResult, error) {
	return mediaResult{}, errors.New("stub")
}
func (m *mediaService) handleRetry(evt *events.MediaRetry)                {}
func mediaFileName(id, orig, mediaType string) (string, error)            { return "", errors.New("stub") }
func mediaDirectPath(msg *waProto.Message) (string, string)               { return "", "" }
func (b *bridgeEvents) markReady()                                        {}
func migrateDirectPath(tx *sql.Tx, _ *Identity, _ *migrationReport) error { return errors.New("stub") }
