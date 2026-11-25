package proxy

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"math"
	"strings"
)

// ---------------------------------------------------------------------------
// AWS event-stream framing
// [total_len:4][headers_len:4][prelude_crc:4][headers][payload][message_crc:4]
// ---------------------------------------------------------------------------

const (
	hvBoolTrue  = 0
	hvBoolFalse = 1
	hvByte      = 2
	hvShort     = 3
	hvInt       = 4
	hvLong      = 5
	hvBytes     = 6
	hvString    = 7
	hvTimestamp = 8
	hvUUID      = 9
)

func eventStreamEncode(headers map[string]string, payload []byte) []byte {
	var hb []byte
	for name, value := range headers {
		nb := []byte(name)
		vb := []byte(value)
		hb = append(hb, byte(len(nb)))
		hb = append(hb, nb...)
		hb = append(hb, hvString, byte(len(vb)>>8), byte(len(vb)))
		hb = append(hb, vb...)
	}
	total := 4 + 4 + 4 + len(hb) + len(payload) + 4
	msg := make([]byte, total)
	binary.BigEndian.PutUint32(msg[0:4], uint32(total))
	binary.BigEndian.PutUint32(msg[4:8], uint32(len(hb)))
	binary.BigEndian.PutUint32(msg[8:12], crc32.ChecksumIEEE(msg[0:8]))
	copy(msg[12:], hb)
	copy(msg[12+len(hb):], payload)
	binary.BigEndian.PutUint32(msg[total-4:], crc32.ChecksumIEEE(msg[:total-4]))
	return msg
}

type eventStreamMessage struct {
	Headers map[string]any
	Payload []byte
}

// eventStreamDecodeAll decodes every complete message in buf and returns the
// number of bytes consumed by complete frames.
func eventStreamDecodeAll(buf []byte) (msgs []eventStreamMessage, consumed int, err error) {
	pos := 0
	for pos+16 <= len(buf) {
		total := int(binary.BigEndian.Uint32(buf[pos : pos+4]))
		if total < 16 || total > 16*1024*1024 {
			return msgs, pos, fmt.Errorf("invalid event-stream frame length %d", total)
		}
		if pos+total > len(buf) {
			break
		}
		headersLen := int(binary.BigEndian.Uint32(buf[pos+4 : pos+8]))
		hstart := pos + 12
		hend := hstart + headersLen
		if hend > pos+total-4 || hend < hstart {
			return msgs, pos, fmt.Errorf("invalid event-stream header length %d", headersLen)
		}
		headers, err := decodeEventHeaders(buf[hstart:hend])
		if err != nil {
			return msgs, pos, err
		}
		payload := append([]byte(nil), buf[hend:pos+total-4]...)
		msgs = append(msgs, eventStreamMessage{Headers: headers, Payload: payload})
		pos += total
	}
	return msgs, pos, nil
}

func decodeEventHeaders(b []byte) (map[string]any, error) {
	out := make(map[string]any)
	i := 0
	for i < len(b) {
		nameLen := int(b[i])
		i++
		if i+nameLen > len(b) {
			return nil, fmt.Errorf("truncated event header name")
		}
		name := string(b[i : i+nameLen])
		i += nameLen
		if i >= len(b) {
			return nil, fmt.Errorf("truncated event header type")
		}
		typ := b[i]
		i++
		switch typ {
		case hvBoolTrue:
			out[name] = true
		case hvBoolFalse:
			out[name] = false
		case hvByte:
			if i+1 > len(b) {
				return nil, fmt.Errorf("truncated byte header")
			}
			out[name] = b[i]
			i++
		case hvShort:
			if i+2 > len(b) {
				return nil, fmt.Errorf("truncated short header")
			}
			out[name] = int(binary.BigEndian.Uint16(b[i : i+2]))
			i += 2
		case hvInt:
			if i+4 > len(b) {
				return nil, fmt.Errorf("truncated int header")
			}
			out[name] = binary.BigEndian.Uint32(b[i : i+4])
			i += 4
		case hvLong, hvTimestamp:
			if i+8 > len(b) {
				return nil, fmt.Errorf("truncated long header")
			}
			out[name] = binary.BigEndian.Uint64(b[i : i+8])
			i += 8
		case hvBytes:
			if i+2 > len(b) {
				return nil, fmt.Errorf("truncated bytes header")
			}
			n := int(binary.BigEndian.Uint16(b[i : i+2]))
			i += 2
			if i+n > len(b) {
				return nil, fmt.Errorf("truncated bytes header value")
			}
			out[name] = append([]byte(nil), b[i:i+n]...)
			i += n
		case hvString:
			if i+2 > len(b) {
				return nil, fmt.Errorf("truncated string header")
			}
			n := int(binary.BigEndian.Uint16(b[i : i+2]))
			i += 2
			if i+n > len(b) {
				return nil, fmt.Errorf("truncated string header value")
			}
			out[name] = string(b[i : i+n])
			i += n
		case hvUUID:
			if i+16 > len(b) {
				return nil, fmt.Errorf("truncated uuid header")
			}
			out[name] = append([]byte(nil), b[i:i+16]...)
			i += 16
		default:
			return nil, fmt.Errorf("unsupported event header type %d", typ)
		}
	}
	return out, nil
}

func headerString(headers map[string]any, key string) string {
	switch v := headers[key].(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return ""
	}
}

// ---------------------------------------------------------------------------
// Minimal CBOR (RPC v2). Covers the shapes used by the Gateway bidi protocol.
// ---------------------------------------------------------------------------

func cborEncode(v any) ([]byte, error) {
	var out []byte
	if err := cborEncodeInto(v, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func cborEncodeHead(major byte, n uint64, out *[]byte) {
	switch {
	case n < 24:
		*out = append(*out, major<<5|byte(n))
	case n <= math.MaxUint8:
		*out = append(*out, major<<5|24, byte(n))
	case n <= math.MaxUint16:
		*out = append(*out, major<<5|25, byte(n>>8), byte(n))
	case n <= math.MaxUint32:
		*out = append(*out, major<<5|26, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	default:
		*out = append(*out, major<<5|27, byte(n>>56), byte(n>>48), byte(n>>40), byte(n>>32),
			byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
}

func cborEncodeInto(v any, out *[]byte) error {
	switch x := v.(type) {
	case nil:
		*out = append(*out, 0xf6)
	case bool:
		if x {
			*out = append(*out, 0xf5)
		} else {
			*out = append(*out, 0xf4)
		}
	case int:
		if x >= 0 {
			cborEncodeHead(0, uint64(x), out)
		} else {
			cborEncodeHead(1, uint64(-1-x), out)
		}
	case int64:
		if x >= 0 {
			cborEncodeHead(0, uint64(x), out)
		} else {
			cborEncodeHead(1, uint64(-1-x), out)
		}
	case uint64:
		cborEncodeHead(0, x, out)
	case string:
		cborEncodeHead(3, uint64(len(x)), out)
		*out = append(*out, x...)
	case []byte:
		cborEncodeHead(2, uint64(len(x)), out)
		*out = append(*out, x...)
	case []any:
		cborEncodeHead(4, uint64(len(x)), out)
		for _, item := range x {
			if err := cborEncodeInto(item, out); err != nil {
				return err
			}
		}
	case map[string]any:
		cborEncodeHead(5, uint64(len(x)), out)
		for k, val := range x {
			if err := cborEncodeInto(k, out); err != nil {
				return err
			}
			if err := cborEncodeInto(val, out); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("cannot CBOR-encode %T", v)
	}
	return nil
}

func cborDecode(data []byte) (any, error) {
	v, _, err := cborDecodeAt(data, 0)
	return v, err
}

func cborDecodeAt(data []byte, i int) (any, int, error) {
	if i >= len(data) {
		return nil, i, fmt.Errorf("truncated CBOR")
	}
	initial := data[i]
	i++
	major := initial >> 5
	info := initial & 0x1f
	var n uint64
	switch {
	case info < 24:
		n = uint64(info)
	case info == 24:
		if i+1 > len(data) {
			return nil, i, fmt.Errorf("truncated CBOR uint8")
		}
		n = uint64(data[i])
		i++
	case info == 25:
		if i+2 > len(data) {
			return nil, i, fmt.Errorf("truncated CBOR uint16")
		}
		n = uint64(binary.BigEndian.Uint16(data[i : i+2]))
		i += 2
	case info == 26:
		if i+4 > len(data) {
			return nil, i, fmt.Errorf("truncated CBOR uint32")
		}
		n = uint64(binary.BigEndian.Uint32(data[i : i+4]))
		i += 4
	case info == 27:
		if i+8 > len(data) {
			return nil, i, fmt.Errorf("truncated CBOR uint64")
		}
		n = binary.BigEndian.Uint64(data[i : i+8])
		i += 8
	default:
		return nil, i, fmt.Errorf("unsupported CBOR additional info %d", info)
	}

	switch major {
	case 0:
		return n, i, nil
	case 1:
		return -1 - int64(n), i, nil
	case 2:
		if i+int(n) > len(data) {
			return nil, i, fmt.Errorf("truncated CBOR bytes")
		}
		out := append([]byte(nil), data[i:i+int(n)]...)
		return out, i + int(n), nil
	case 3:
		if i+int(n) > len(data) {
			return nil, i, fmt.Errorf("truncated CBOR string")
		}
		return string(data[i : i+int(n)]), i + int(n), nil
	case 4:
		arr := make([]any, 0, n)
		for k := uint64(0); k < n; k++ {
			item, ni, err := cborDecodeAt(data, i)
			if err != nil {
				return nil, ni, err
			}
			arr = append(arr, item)
			i = ni
		}
		return arr, i, nil
	case 5:
		m := make(map[string]any, n)
		for k := uint64(0); k < n; k++ {
			key, ni, err := cborDecodeAt(data, i)
			if err != nil {
				return nil, ni, err
			}
			i = ni
			val, ni, err := cborDecodeAt(data, i)
			if err != nil {
				return nil, ni, err
			}
			i = ni
			m[fmt.Sprint(key)] = val
		}
		return m, i, nil
	case 6:
		// Tag: decode the tagged value and ignore the tag itself.
		return cborDecodeAt(data, i)
	case 7:
		switch info {
		case 20:
			return false, i, nil
		case 21:
			return true, i, nil
		case 22:
			return nil, i, nil
		case 25:
			return float64(math.Float32frombits(uint32(n))), i, nil
		case 26:
			return math.Float64frombits(n), i, nil
		}
		return nil, i, fmt.Errorf("unsupported CBOR simple value %d", info)
	}
	return nil, i, fmt.Errorf("unsupported CBOR major type %d", major)
}

// ---------------------------------------------------------------------------
// AIDP (enterprise/social) bidi codec.
//
// Outbound: UTF-8 JSON {"eventType":"rawInputEvent","payload":{"payload":b64}}
// Inbound:  AWS event-stream whose body is {"payload": b64(json)}
// ---------------------------------------------------------------------------

func aidpEncode(jsonStr string) []byte {
	b64 := base64.StdEncoding.EncodeToString([]byte(jsonStr))
	outer, _ := json.Marshal(map[string]any{
		"eventType": "rawInputEvent",
		"payload":   map[string]any{"payload": b64},
	})
	return outer
}

func aidpDecode(data []byte) ([]string, error) {
	msgs, _, err := eventStreamDecodeAll(data)
	if len(msgs) == 0 {
		// Some servers may answer with a plain JSON text frame.
		var probe map[string]any
		if json.Unmarshal(data, &probe) == nil {
			return []string{string(data)}, nil
		}
		return nil, fmt.Errorf("decode AIDP event-stream: %v", err)
	}
	out := make([]string, 0, len(msgs))
	var firstErr error
	for _, msg := range msgs {
		if mt := headerString(msg.Headers, ":message-type"); mt == "error" || mt == "exception" {
			code := headerString(msg.Headers, ":error-code")
			if code == "" {
				code = headerString(msg.Headers, ":exception-type")
			}
			detail := headerString(msg.Headers, ":error-message")
			if firstErr == nil {
				firstErr = fmt.Errorf("upstream error [%s] %s", code, detail)
			}
			continue
		}
		var outer struct {
			Payload string `json:"payload"`
		}
		if err := json.Unmarshal(msg.Payload, &outer); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("decode AIDP body: %w", err)
			}
			continue
		}
		if outer.Payload == "" {
			if firstErr == nil {
				firstErr = fmt.Errorf("AIDP body has no payload")
			}
			continue
		}
		inner, err := base64.StdEncoding.DecodeString(outer.Payload)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("decode AIDP payload base64: %w", err)
			}
			continue
		}
		out = append(out, string(inner))
	}
	if len(out) == 0 && firstErr == nil {
		firstErr = err
	}
	return out, firstErr
}

// ---------------------------------------------------------------------------
// Gateway (gateway/social_gateway) bidi codec.
// Outbound: event-stream + CBOR {"payload": json_bytes}
// Inbound:  event-stream + CBOR {"payload": json_bytes}
// ---------------------------------------------------------------------------

func gatewayEncode(jsonStr string) []byte {
	inner, err := cborEncode(map[string]any{"payload": []byte(jsonStr)})
	if err != nil {
		return nil
	}
	return eventStreamEncode(map[string]string{
		":event-type":   "rawInputEvent",
		":content-type": "application/cbor",
		":message-type": "event",
	}, inner)
}

func gatewayDecode(data []byte) ([]string, error) {
	msgs, _, err := eventStreamDecodeAll(data)
	if len(msgs) == 0 {
		return nil, fmt.Errorf("decode gateway event-stream: %v", err)
	}
	out := make([]string, 0, len(msgs))
	var firstErr error
	for _, msg := range msgs {
		if mt := headerString(msg.Headers, ":message-type"); mt == "error" || mt == "exception" {
			code := headerString(msg.Headers, ":error-code")
			if code == "" {
				code = headerString(msg.Headers, ":exception-type")
			}
			detail := headerString(msg.Headers, ":error-message")
			if firstErr == nil {
				firstErr = fmt.Errorf("upstream error [%s] %s", code, detail)
			}
			continue
		}
		val, err := cborDecode(msg.Payload)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("decode gateway CBOR: %w", err)
			}
			continue
		}
		m, ok := val.(map[string]any)
		if !ok {
			if firstErr == nil {
				firstErr = fmt.Errorf("gateway CBOR is not a map")
			}
			continue
		}
		switch inner := m["payload"].(type) {
		case []byte:
			out = append(out, string(inner))
		case string:
			out = append(out, inner)
		default:
			if firstErr == nil {
				firstErr = fmt.Errorf("gateway CBOR has no payload")
			}
		}
	}
	if len(out) == 0 && firstErr == nil {
		firstErr = err
	}
	return out, firstErr
}

func isGatewayProvider(provider string) bool {
	p := strings.ToLower(provider)
	return p == "gateway" || p == "social_gateway"
}
