package common

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

var (
	// ErrLineTooLong 表示 JSON 行在 LF 前已超过配置的字节上限。
	ErrLineTooLong = errors.New("JSON line exceeds maximum size")
	// ErrUnterminatedLine 表示读到 EOF 时已有内容，但末尾没有 LF。
	ErrUnterminatedLine = errors.New("JSON line is not terminated by LF")
)

// LineReader 读取有长度上限的 JSON Lines，并将超长行持续丢弃到 LF。
type LineReader struct {
	reader   *bufio.Reader
	maxBytes int
}

// NewLineReader 创建有界行读取器；maxBytes 只计算 LF 前的字节。
func NewLineReader(reader io.Reader, maxBytes int) (*LineReader, error) {
	if reader == nil {
		return nil, errors.New("reader is nil")
	}
	if maxBytes <= 0 {
		return nil, errors.New("maxBytes must be positive")
	}
	bufferSize := maxBytes + 1
	if bufferSize > 64*1024 {
		bufferSize = 64 * 1024
	}
	return &LineReader{reader: bufio.NewReaderSize(reader, bufferSize), maxBytes: maxBytes}, nil
}

// ReadLine 返回下一条非空且以 LF 结束的行，结果不包含 LF。
func (reader *LineReader) ReadLine() ([]byte, error) {
	for {
		line, err := reader.readRawLine()
		if err != nil {
			return nil, err
		}
		if len(line) == 0 {
			continue
		}
		return line, nil
	}
}

// readRawLine 读取一条物理行，并在超限时持续消费到 LF，避免下一次读取从半行开始。
func (reader *LineReader) readRawLine() ([]byte, error) {
	line := make([]byte, 0, min(reader.maxBytes, 4096))
	total := 0
	tooLong := false
	for {
		fragment, err := reader.reader.ReadSlice('\n')
		terminated := err == nil
		body := fragment
		if terminated {
			body = fragment[:len(fragment)-1]
		}
		total += len(body)
		if total > reader.maxBytes {
			tooLong = true
		}
		// 一旦超限便不再保留内容，只消费后续片段直到该物理行结束。
		if !tooLong {
			line = append(line, body...)
		}

		switch {
		case terminated:
			if tooLong {
				return nil, ErrLineTooLong
			}
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if total == 0 {
				return nil, io.EOF
			}
			if tooLong {
				return nil, ErrLineTooLong
			}
			return nil, ErrUnterminatedLine
		default:
			return nil, fmt.Errorf("read JSON line: %w", err)
		}
	}
}
