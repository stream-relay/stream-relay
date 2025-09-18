package main

// Frame schema shared by handlers and Redis stream encoding.

type OutFrame struct {
	Type              string `json:"type" bson:"type"`                               // "delta" | "error"
	StreamID          string `json:"streamId" bson:"streamId"`
	DataB64           string `json:"dataB64,omitempty" bson:"dataB64,omitempty"`
	OriginStatus      int    `json:"originStatus,omitempty" bson:"originStatus,omitempty"`
	OriginContentType string `json:"originContentType,omitempty" bson:"originContentType,omitempty"`
	Error             string `json:"error,omitempty" bson:"error,omitempty"`
	Done              bool   `json:"done,omitempty" bson:"done,omitempty"`
}

type InFrame struct {
	Type     string        `json:"type" bson:"type"`                   // "start" | "ack" | "cancel"
	StreamID string        `json:"streamId" bson:"streamId"`
	Payload  *StartPayload `json:"payload,omitempty" bson:"payload,omitempty"` // for start
}

// BodyDict represents JSON-aware deduplication for LLM request bodies.
// Large JSON values (>1KB) are extracted, hashed, and replaced with {"$ref": "sha256:..."}
// The skeleton contains the structure with refs, put contains new chunks.
// v1: Put values are base64-encoded strings (JSON)
// v2: Put values are raw binary (BSON Binary type)
type BodyDict struct {
	V        int                    `json:"v" bson:"v"`               // version: 1=base64, 2=binary
	Skeleton string                 `json:"skeleton" bson:"skeleton"` // JSON string with $ref placeholders
	Put      map[string]interface{} `json:"put" bson:"put"`           // hash -> base64 string (v1) or binary (v2)
}

type StartPayload struct {
	Target   string                 `json:"target" bson:"target"`
	Method   string                 `json:"method" bson:"method"`
	Headers  map[string]interface{} `json:"headers,omitempty" bson:"headers,omitempty"`
	BodyB64  string                 `json:"bodyB64,omitempty" bson:"bodyB64,omitempty"`   // full body (fallback)
	BodyDict *BodyDict              `json:"bodyDict,omitempty" bson:"bodyDict,omitempty"` // JSON-aware deduplication
}
