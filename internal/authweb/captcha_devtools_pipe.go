package authweb

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

const chromeDevToolsPipeMessageLimit = 4 << 20

type chromeDevToolsTransport interface {
	ReadJSON(any) error
	WriteJSON(any) error
}

type chromeDevToolsPipe struct {
	reader    io.ReadCloser
	writer    io.WriteCloser
	buffered  *bufio.Reader
	writeMu   sync.Mutex
	closeOnce sync.Once
}

type chromeDevToolsPipeSetup struct {
	host       *chromeDevToolsPipe
	childRead  *os.File
	childWrite *os.File
}

func newChromeDevToolsPipe(reader io.ReadCloser, writer io.WriteCloser) *chromeDevToolsPipe {
	return &chromeDevToolsPipe{
		reader:   reader,
		writer:   writer,
		buffered: bufio.NewReaderSize(reader, 64<<10),
	}
}

func (p *chromeDevToolsPipe) WriteJSON(value any) error {
	if p == nil || p.writer == nil {
		return errors.New("private Chrome DevTools pipe is unavailable")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(encoded) > chromeDevToolsPipeMessageLimit {
		return errors.New("Chrome DevTools command exceeds limit")
	}
	encoded = append(encoded, 0)
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_, err = p.writer.Write(encoded)
	return err
}

func (p *chromeDevToolsPipe) ReadJSON(value any) error {
	if p == nil || p.buffered == nil {
		return errors.New("private Chrome DevTools pipe is unavailable")
	}
	var message bytes.Buffer
	for {
		fragment, err := p.buffered.ReadSlice(0)
		if len(fragment) > 0 {
			if message.Len()+len(fragment) > chromeDevToolsPipeMessageLimit+1 {
				return errors.New("Chrome DevTools response exceeds limit")
			}
			_, _ = message.Write(fragment)
		}
		switch {
		case err == nil:
			raw := message.Bytes()
			if len(raw) == 0 || raw[len(raw)-1] != 0 {
				return errors.New("invalid Chrome DevTools pipe frame")
			}
			return json.Unmarshal(raw[:len(raw)-1], value)
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && message.Len() == 0:
			return io.EOF
		default:
			return fmt.Errorf("read Chrome DevTools pipe: %w", err)
		}
	}
}

func (p *chromeDevToolsPipe) Close() error {
	if p == nil {
		return nil
	}
	var closeErr error
	p.closeOnce.Do(func() {
		closeErr = errors.Join(p.reader.Close(), p.writer.Close())
	})
	return closeErr
}

func (s *chromeDevToolsPipeSetup) closeChildEnds() {
	if s == nil {
		return
	}
	if s.childRead != nil {
		_ = s.childRead.Close()
		s.childRead = nil
	}
	if s.childWrite != nil {
		_ = s.childWrite.Close()
		s.childWrite = nil
	}
}

func (s *chromeDevToolsPipeSetup) closeAll() {
	if s == nil {
		return
	}
	s.closeChildEnds()
	_ = s.host.Close()
}
