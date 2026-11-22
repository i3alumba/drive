//go:build !linux

package torrent

import (
	"context"
	"errors"

	"remote-drive/api/internal/mediaadmission"
)

// The local converter requires Linux process-group and owned-descriptor semantics.
type NativeConverter struct{}

func NewNativeConverter(*mediaadmission.Admission) (*NativeConverter, error) {
	return nil, errors.New("conversion_unavailable")
}
func (*NativeConverter) Convert(context.Context, string, string) error {
	return errors.New("conversion_unavailable")
}
func (*Manager) ConfigureConverter(Converter) {}
func (*Manager) ConversionAvailable() bool    { return false }
func (m *Manager) convertCompleted(id, _, _ string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if job := m.jobs[id]; job != nil {
		job.ConversionStatus = "failed"
		job.ConversionResults = []ConversionResult{{Status: "failed", Error: "conversion_unavailable"}}
	}
}
