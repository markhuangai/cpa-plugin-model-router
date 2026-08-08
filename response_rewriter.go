package main

import (
	"bytes"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var responseModelPaths = []string{"model", "modelVersion", "response.model", "response.modelVersion", "message.model"}

const maxPendingStreamBytes = 1 << 20

func rewriteResponseModel(body []byte, model string) []byte {
	if stringsTrimmed := bytes.TrimSpace(body); model == "" || len(stringsTrimmed) == 0 || !gjson.ValidBytes(stringsTrimmed) {
		return body
	}
	out := body
	for _, path := range responseModelPaths {
		if gjson.GetBytes(out, path).Exists() {
			if updated, err := sjson.SetBytes(out, path, model); err == nil {
				out = updated
			}
		}
	}
	return out
}

type streamModelRewriter struct {
	model   string
	pending []byte
}

func newStreamModelRewriter(model string) *streamModelRewriter {
	return &streamModelRewriter{model: model}
}

func (rewriter *streamModelRewriter) Rewrite(chunk []byte) []byte {
	if rewriter == nil || rewriter.model == "" || len(chunk) == 0 {
		return chunk
	}
	if len(rewriter.pending) > 0 {
		combined := make([]byte, 0, len(rewriter.pending)+len(chunk))
		combined = append(combined, rewriter.pending...)
		combined = append(combined, chunk...)
		chunk = combined
		rewriter.pending = nil
	}
	chunk = normalizeGluedSSEEvents(chunk)
	trimmed := bytes.TrimSpace(chunk)
	if len(trimmed) > 0 && trimmed[0] == '{' && gjson.ValidBytes(trimmed) {
		return rewriteResponseModel(trimmed, rewriter.model)
	}
	if len(chunk) > maxPendingStreamBytes {
		return chunk
	}
	complete, remainder := splitCompleteSSE(chunk)
	if len(complete) == 0 {
		if payload := lastSSEDataPayload(chunk); gjson.ValidBytes(payload) {
			return rewriteSSELines(chunk, rewriter.model)
		}
		rewriter.pending = append([]byte(nil), chunk...)
		return nil
	}
	rewriter.pending = append([]byte(nil), remainder...)
	return rewriteSSELines(complete, rewriter.model)
}

func (rewriter *streamModelRewriter) Finish() []byte {
	if rewriter == nil || len(rewriter.pending) == 0 {
		return nil
	}
	pending := rewriter.pending
	rewriter.pending = nil
	return rewriteSSELines(pending, rewriter.model)
}

func splitCompleteSSE(chunk []byte) ([]byte, []byte) {
	lastLF := bytes.LastIndex(chunk, []byte("\n\n"))
	lastCRLF := bytes.LastIndex(chunk, []byte("\r\n\r\n"))
	index, width := lastLF, 2
	if lastCRLF > lastLF {
		index, width = lastCRLF, 4
	}
	if index < 0 {
		return nil, chunk
	}
	end := index + width
	return chunk[:end], chunk[end:]
}

func rewriteSSELines(payload []byte, model string) []byte {
	lines := bytes.Split(payload, []byte("\n"))
	for index, line := range lines {
		prefix, data, ok := sseDataLine(line)
		if !ok || len(data) == 0 || data[0] != '{' || !gjson.ValidBytes(data) {
			continue
		}
		lines[index] = append(append([]byte(nil), prefix...), rewriteResponseModel(data, model)...)
	}
	return bytes.Join(lines, []byte("\n"))
}

func lastSSEDataPayload(chunk []byte) []byte {
	lines := bytes.Split(chunk, []byte("\n"))
	for index := len(lines) - 1; index >= 0; index-- {
		if _, data, ok := sseDataLine(lines[index]); ok && len(data) > 0 {
			return data
		}
	}
	return nil
}

func sseDataLine(line []byte) ([]byte, []byte, bool) {
	if data, ok := bytes.CutPrefix(line, []byte("data: ")); ok {
		return []byte("data: "), data, true
	}
	if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
		return []byte("data:"), data, true
	}
	return nil, nil, false
}

func normalizeGluedSSEEvents(chunk []byte) []byte {
	chunk = replaceGluedSSE(chunk, []byte("}event:"), []byte("}\n\nevent:"))
	chunk = replaceGluedSSE(chunk, []byte("}\r\nevent:"), []byte("}\r\n\r\nevent:"))
	chunk = replaceGluedSSE(chunk, []byte("}data:"), []byte("}\ndata:"))
	return replaceGluedSSE(chunk, []byte("}\r\ndata:"), []byte("}\r\ndata:"))
}

func replaceGluedSSE(chunk, old, replacement []byte) []byte {
	if len(chunk) == 0 || !bytes.Contains(chunk, old) {
		return chunk
	}
	var out []byte
	remaining := chunk
	for {
		index := bytes.Index(remaining, old)
		if index < 0 {
			return append(out, remaining...)
		}
		lineStart := bytes.LastIndexByte(remaining[:index], '\n')
		candidate := remaining[:index+1]
		if lineStart >= 0 {
			candidate = remaining[lineStart+1 : index+1]
		}
		_, data, ok := sseDataLine(candidate)
		if ok && len(data) > 0 && gjson.ValidBytes(data) {
			out = append(out, remaining[:index]...)
			out = append(out, replacement...)
			remaining = remaining[index+len(old):]
			continue
		}
		out = append(out, remaining[:index+len(old)]...)
		remaining = remaining[index+len(old):]
	}
}
