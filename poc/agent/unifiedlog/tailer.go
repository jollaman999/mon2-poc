package unifiedlog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"go.opentelemetry.io/collector/consumer/consumererror"
	"go.uber.org/zap"
)

const (
	defaultPollInterval = 200 * time.Millisecond
	readChunk           = 64 << 10
	maxLineBytes        = 1 << 20
	maxBatchLines       = 1000
	posSaveInterval     = time.Second
)

type tailLine struct {
	text string
	ts   time.Time // 처음 읽은 시각. 다시 넘길 때도 처음 값을 유지한다.
	n    int       // 파일에서 차지한 바이트 수 (줄바꿈 포함)
}

// filePos 는 storage 에 남기는 파일 위치다. ID 는 inode("<dev>:<ino>"), Windows 는 비어 있다.
type filePos struct {
	ID     string `json:"id,omitempty"`
	Offset int64  `json:"offset"`
}

// tailer 는 파일 하나를 주기적으로 읽어 줄 단위로 넘긴다.
// 다음 부품이 받아 간 줄까지만 위치를 올리고, 받지 못하면(영구 오류가 아니면) 다음 주기에 같은 자리부터 다시 읽는다.
// handle 은 끝의 hold 줄을 아직 쓰지 않았다고(여러 줄 묶음이 덜 찼다고) 돌려줄 수 있다. 그 줄은 위치에 넣지 않고
// 다음 주기에 새 줄과 함께 다시 넘긴다. final 이면(회전 · 멈춤) 남기지 말고 다 써야 한다.
// 이름이 다른 파일로 바뀌면(회전) 남은 것을 다 읽고 새 파일을 처음부터, 크기가 줄면(copytruncate) 처음부터 읽는다.
type tailer struct {
	path      string
	fromStart bool // 저장된 위치가 없을 때 처음부터 읽을지
	store     *posStore
	logger    *zap.Logger
	poll      time.Duration
	handle    func(ctx context.Context, lines []tailLine, final bool) (hold int, err error)

	f        *os.File
	fi       os.FileInfo
	off      int64       // 다음 부품에 넘긴 데까지
	pending  []byte      // off 부터 읽었지만 아직 위치에 넣지 않은 바이트
	heldTS   []time.Time // pending 앞쪽의 남겨 둔 줄들이 처음 읽힌 시각
	tried    bool
	dirty    bool
	lastSave time.Time
}

func (t *tailer) key() string { return "file:" + t.path }

func (t *tailer) run(ctx context.Context) {
	defer t.finish()
	poll := t.poll
	if poll <= 0 {
		poll = defaultPollInterval
	}
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		if t.f == nil {
			t.open(ctx)
		}
		if t.f != nil {
			t.readAvailable(ctx)
			t.checkRotation(ctx)
		}
		t.save(ctx, false)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// open 은 파일을 열고 시작 위치를 정한다.
// 처음 여는 것이면 저장된 위치 -> from_beginning 순으로 정하고, 회전 뒤나 처음에 없던 파일이 생긴 것이면 처음부터 읽는다.
func (t *tailer) open(ctx context.Context) {
	first := !t.tried
	t.tried = true
	f, err := os.Open(t.path)
	if err != nil {
		return
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return
	}
	t.f, t.fi, t.pending, t.heldTS = f, fi, nil, nil
	if !first {
		t.off = 0
		t.dirty = true
		return
	}
	if b, err := t.store.get(ctx, t.key()); err == nil && len(b) > 0 {
		var p filePos
		if json.Unmarshal(b, &p) == nil {
			if p.ID == fileID(fi) && p.Offset <= fi.Size() {
				t.off = p.Offset
			} else {
				// 멈춘 사이에 파일이 바뀌었거나 잘렸다. 지금 파일은 처음부터 본 적이 없다.
				t.off = 0
				t.dirty = true
			}
			return
		}
	}
	if t.fromStart {
		t.off = 0
	} else {
		t.off = fi.Size()
	}
	t.dirty = true
}

// readAvailable 는 파일 끝까지 읽어 끝난 줄을 넘긴다. 다음 부품이 받지 못해 멈췄으면 false.
func (t *tailer) readAvailable(ctx context.Context) bool {
	buf := make([]byte, readChunk)
	for {
		n, rerr := t.f.ReadAt(buf, t.off+int64(len(t.pending)))
		if n > 0 {
			t.pending = append(t.pending, buf[:n]...)
		}
		for {
			// 줄바꿈 없이 너무 길면 끝 조각도 한 줄로 본다.
			lines, used := t.split(lastLineTooLong(t.pending))
			if len(lines) == 0 {
				break
			}
			consumed, ok := t.deliver(ctx, lines, used, false)
			if !ok {
				return false
			}
			if consumed == 0 {
				break
			}
		}
		if n == 0 || rerr != nil {
			if rerr != nil && !errors.Is(rerr, io.EOF) {
				t.logger.Debug("파일 읽기 실패", zap.String("path", t.path), zap.Error(rerr))
			}
			return true
		}
	}
}

// split 은 pending 을 줄로 나누고, 앞쪽의 남겨 둔 줄에는 처음 읽은 시각을 다시 붙인다.
func (t *tailer) split(partial bool) ([]tailLine, int) {
	lines, used := splitLines(t.pending, maxBatchLines, partial)
	for i := 0; i < len(lines) && i < len(t.heldTS); i++ {
		lines[i].ts = t.heldTS[i]
	}
	return lines, used
}

func lastLineTooLong(b []byte) bool {
	return len(b)-(bytes.LastIndexByte(b, '\n')+1) >= maxLineBytes
}

// deliver 는 줄을 넘기고, 다 쓴 줄만큼 위치를 올린다. 쓴 줄 수와, 다음 부품이 받았는지를 돌려준다.
func (t *tailer) deliver(ctx context.Context, lines []tailLine, used int, final bool) (int, bool) {
	hold, err := t.handle(ctx, lines, final)
	if err != nil {
		if !consumererror.IsPermanent(err) {
			t.logger.Warn("로그를 넘기지 못해 다음 주기에 다시 읽는다", zap.String("path", t.path), zap.Error(err))
			t.pending, t.heldTS = nil, nil
			return 0, false
		}
		// 영구 오류는 다시 보내도 같다. 버리고 넘어간다.
		hold = 0
	}
	if hold < 0 || final {
		hold = 0
	}
	if hold > len(lines) {
		hold = len(lines)
	}
	consumedLines := len(lines) - hold
	consumed := used
	t.heldTS = t.heldTS[:0]
	for _, l := range lines[consumedLines:] {
		consumed -= l.n
		t.heldTS = append(t.heldTS, l.ts)
	}
	if consumed > 0 {
		t.off += int64(consumed)
		t.pending = append([]byte(nil), t.pending[consumed:]...)
		t.dirty = true
	}
	return consumedLines, true
}

// checkRotation 은 경로가 다른 파일을 가리키게 됐는지, 파일이 잘렸는지 본다.
func (t *tailer) checkRotation(ctx context.Context) {
	fi, err := os.Stat(t.path)
	if err != nil {
		// 지금은 경로에 파일이 없다. 열어 둔 파일을 계속 들고 있다가 새 파일이 생기면 바꾼다.
		return
	}
	if os.SameFile(t.fi, fi) {
		if fi.Size() < t.off+int64(len(t.pending)) {
			t.logger.Debug("파일이 잘려 처음부터 읽는다", zap.String("path", t.path))
			t.off, t.pending, t.heldTS, t.dirty = 0, nil, nil, true
		}
		return
	}
	// 회전: 옛 파일을 끝까지 읽고, 남겨 둔 줄과 줄바꿈 없이 남은 끝 조각까지 다 넘긴다. 넘기지 못했으면 다음 주기에 다시 한다.
	if !t.readAvailable(ctx) {
		return
	}
	if len(t.pending) > 0 {
		lines, used := t.split(true)
		if _, ok := t.deliver(ctx, lines, used, true); !ok {
			return
		}
	}
	_ = t.f.Close()
	t.f = nil
	t.logger.Debug("파일이 바뀌어 새 파일을 처음부터 읽는다", zap.String("path", t.path))
	t.open(ctx)
	if t.f != nil {
		t.readAvailable(ctx)
	}
}

func (t *tailer) save(ctx context.Context, force bool) {
	if !t.store.enabled() || !t.dirty || t.fi == nil {
		return
	}
	if !force && time.Since(t.lastSave) < posSaveInterval {
		return
	}
	b, _ := json.Marshal(filePos{ID: fileID(t.fi), Offset: t.off})
	if err := t.store.set(ctx, t.key(), b); err != nil {
		t.logger.Warn("파일 위치를 남기지 못했다", zap.String("path", t.path), zap.Error(err))
		return
	}
	t.dirty = false
	t.lastSave = time.Now()
}

// finish 는 멈출 때 남겨 둔 줄(덜 찬 여러 줄 묶음)을 내보내고 위치를 남긴다.
// 줄바꿈 없는 끝 조각은 아직 덜 쓰인 줄일 수 있어 넘기지 않는다. 다음 시작 때 그 자리부터 다시 읽는다.
func (t *tailer) finish() {
	if t.f != nil && len(t.pending) > 0 {
		if lines, used := t.split(false); len(lines) > 0 {
			t.deliver(context.Background(), lines, used, true)
		}
	}
	t.save(context.Background(), true)
	if t.f != nil {
		_ = t.f.Close()
		t.f = nil
	}
}

// splitLines 는 끝난 줄을 max 개까지 잘라 내고, 쓴 바이트 수(줄바꿈 포함)를 돌려준다.
// partial 이면 줄바꿈 없이 남은 끝 조각도 한 줄로 붙인다.
func splitLines(b []byte, max int, partial bool) ([]tailLine, int) {
	var out []tailLine
	used := 0
	now := time.Now()
	for len(out) < max {
		i := bytes.IndexByte(b[used:], '\n')
		if i < 0 {
			if partial && used < len(b) {
				out = append(out, tailLine{text: string(bytes.TrimRight(b[used:], "\r")), ts: now, n: len(b) - used})
				used = len(b)
			}
			break
		}
		line := bytes.TrimRight(b[used:used+i], "\r")
		out = append(out, tailLine{text: string(line), ts: now, n: i + 1})
		used += i + 1
	}
	return out, used
}
