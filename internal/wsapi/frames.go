package wsapi

// Server→client frame types. Each carries its own discriminating "type".

type frameNew struct {
	Type string `json:"type"` // "new"
}

type wsMessage struct {
	ID        int64  `json:"id"`
	From      string `json:"from"`
	Payload   string `json:"payload"` // base64
	CreatedAt string `json:"created_at"`
}

type frameMessages struct {
	Type  string      `json:"type"` // "messages"
	Items []wsMessage `json:"items"`
}

type frameAckOK struct {
	Type string  `json:"type"` // "ack_ok"
	IDs  []int64 `json:"ids"`
}

type frameSent struct {
	Type      string `json:"type"` // "sent"
	ID        *int64 `json:"id"`
	Duplicate bool   `json:"duplicate,omitempty"`
}

type frameError struct {
	Type    string `json:"type"` // "error"
	Code    string `json:"code"`
	Message string `json:"message"`
}

func errorFrame(code, msg string) frameError {
	return frameError{Type: "error", Code: code, Message: msg}
}
