package proto

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// JSON-RPC 2.0 error codes (PROTOCOL.md §3.1).
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
	CodeBusy           = -32001
	CodeNotConfigured  = -32002
	CodeCameraError    = -32003
	CodeIOError        = -32004
)

// Version is the JSON-RPC version string.
const Version = "2.0"

// Request is a JSON-RPC request (server → helper).
type Request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// Response is a JSON-RPC response (helper → server). Exactly one of Result
// and Error is set.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// Notification is a JSON-RPC notification (helper → server).
type Notification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// RPCError is a JSON-RPC error object. It implements error.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

// Field returns data.field if present (used with CodeInvalidParams).
func (e *RPCError) Field() string {
	var d struct {
		Field string `json:"field"`
	}
	_ = json.Unmarshal(e.Data, &d)
	return d.Field
}

// Incoming is any message read from ctl.sock, before classification.
type Incoming struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *RPCError       `json:"error"`
}

// IsResponse reports whether m is a response (has id, no method).
func (m *Incoming) IsResponse() bool { return m.ID != nil && m.Method == "" }

// IsNotification reports whether m is a notification (method, no id).
func (m *Incoming) IsNotification() bool { return m.ID == nil && m.Method != "" }

// IsRequest reports whether m is a request (method and id).
func (m *Incoming) IsRequest() bool { return m.ID != nil && m.Method != "" }

// ParseIncoming decodes one line.
func ParseIncoming(line []byte) (*Incoming, error) {
	var m Incoming
	if err := json.Unmarshal(line, &m); err != nil {
		return nil, err
	}
	if m.JSONRPC != Version {
		return nil, fmt.Errorf("proto: bad jsonrpc version %q", m.JSONRPC)
	}
	return &m, nil
}

// MaxLine is the maximum length of one control message.
const MaxLine = 1 << 20

// ErrLineTooLong is returned by LineReader for oversize lines.
var ErrLineTooLong = errors.New("proto: control line too long")

// LineReader reads newline-delimited messages with a size limit.
type LineReader struct {
	r   *bufio.Reader
	max int
}

// NewLineReader wraps r.
func NewLineReader(r io.Reader) *LineReader {
	return &LineReader{r: bufio.NewReaderSize(r, 64<<10), max: MaxLine}
}

// ReadLine returns the next line without the trailing newline.
func (l *LineReader) ReadLine() ([]byte, error) {
	var line []byte
	for {
		chunk, err := l.r.ReadSlice('\n')
		if len(line)+len(chunk) > l.max {
			return nil, ErrLineTooLong
		}
		line = append(line, chunk...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if err == io.EOF && len(line) > 0 {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		return line[:len(line)-1], nil
	}
}

// MarshalLine encodes v as one JSON line.
func MarshalLine(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
