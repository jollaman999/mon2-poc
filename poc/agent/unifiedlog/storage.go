package unifiedlog

import (
	"context"
	"fmt"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/extension/xextension/storage"
)

// posStore 는 읽은 위치를 storage 확장에 남긴다. client 가 nil 이면 아무것도 하지 않는다.
type posStore struct {
	client storage.Client
}

func newPosStore(ctx context.Context, host component.Host, storageID *component.ID, self component.ID) (*posStore, error) {
	if storageID == nil {
		return &posStore{}, nil
	}
	ext, ok := host.GetExtensions()[*storageID]
	if !ok {
		return nil, fmt.Errorf("storage 확장 %s 을 찾지 못했다", storageID)
	}
	se, ok := ext.(storage.Extension)
	if !ok {
		return nil, fmt.Errorf("%s 는 storage 확장이 아니다", storageID)
	}
	client, err := se.GetClient(ctx, component.KindReceiver, self, "")
	if err != nil {
		return nil, fmt.Errorf("storage 확장 %s 에서 클라이언트를 받지 못했다: %w", storageID, err)
	}
	return &posStore{client: client}, nil
}

func (s *posStore) enabled() bool { return s != nil && s.client != nil }

func (s *posStore) get(ctx context.Context, key string) ([]byte, error) {
	if !s.enabled() {
		return nil, nil
	}
	return s.client.Get(ctx, key)
}

func (s *posStore) set(ctx context.Context, key string, v []byte) error {
	if !s.enabled() {
		return nil
	}
	return s.client.Set(ctx, key, v)
}

func (s *posStore) delete(ctx context.Context, key string) error {
	if !s.enabled() {
		return nil
	}
	return s.client.Delete(ctx, key)
}

func (s *posStore) close(ctx context.Context) error {
	if !s.enabled() {
		return nil
	}
	return s.client.Close(ctx)
}
