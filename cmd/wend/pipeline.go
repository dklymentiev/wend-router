package main

import "encoding/json"

// Transform is a step every request and response passes through. The chain
// is empty for now; it is where features such as masking private data on
// the way out and restoring it on the way in plug in, without touching
// routing.
type Transform interface {
	// Request may rewrite the outgoing body before it is sent to a model.
	Request(session string, body map[string]json.RawMessage) error
	// Response may rewrite a complete non-streaming response body.
	Response(session string, body []byte) []byte
	// Chunk may rewrite one streamed SSE line. Returning nil holds the
	// line back (to join a value split across chunks); Flush releases it.
	Chunk(session string, line []byte) []byte
	Flush(session string) []byte
}

func (r *Router) applyRequest(session string, body map[string]json.RawMessage) error {
	for _, t := range r.pipeline {
		if err := t.Request(session, body); err != nil {
			return err
		}
	}
	return nil
}

func (r *Router) applyResponse(session string, b []byte) []byte {
	for _, t := range r.pipeline {
		b = t.Response(session, b)
	}
	return b
}

func (r *Router) applyChunk(session string, line []byte) []byte {
	for _, t := range r.pipeline {
		if line = t.Chunk(session, line); line == nil {
			return nil
		}
	}
	return line
}
