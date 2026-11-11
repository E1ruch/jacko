package realtime

import "encoding/json"

type Event struct {
	Version int             `json:"v"`
	Op      string          `json:"op"`
	ID      int64           `json:"event_id,string"`
	Seq     int64           `json:"seq,string"`
	Type    string          `json:"t"`
	Data    json.RawMessage `json:"d"`
}

type Frame struct {
	Seq     int64
	Payload []byte
}
