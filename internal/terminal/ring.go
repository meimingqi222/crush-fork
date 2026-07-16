package terminal

type byteRing struct {
	data        []byte
	start       int
	length      int
	startOffset uint64
	endOffset   uint64
}

func newByteRing(capacity int) byteRing {
	return byteRing{data: make([]byte, capacity)}
}

func (r *byteRing) append(value []byte) uint64 {
	offset := r.endOffset
	r.endOffset += uint64(len(value))
	if len(r.data) == 0 || len(value) == 0 {
		return offset
	}
	if len(value) >= len(r.data) {
		value = value[len(value)-len(r.data):]
		copy(r.data, value)
		r.start = 0
		r.length = len(value)
		r.startOffset = r.endOffset - uint64(r.length)
		return offset
	}
	overflow := max(0, r.length+len(value)-len(r.data))
	if overflow > 0 {
		r.start = (r.start + overflow) % len(r.data)
		r.length -= overflow
		r.startOffset += uint64(overflow)
	}
	writeAt := (r.start + r.length) % len(r.data)
	first := min(len(value), len(r.data)-writeAt)
	copy(r.data[writeAt:], value[:first])
	copy(r.data, value[first:])
	r.length += len(value)
	return offset
}

func (r *byteRing) snapshot(after uint64, limit int) (data []byte, start, end uint64, truncated bool, ok bool) {
	if after > r.endOffset {
		return nil, 0, 0, false, false
	}
	start = after
	if start < r.startOffset {
		start = r.startOffset
		truncated = true
	}
	end = min(r.endOffset, start+uint64(limit))
	length := int(end - start)
	result := make([]byte, length)
	if length == 0 || len(r.data) == 0 {
		return result, start, end, truncated, true
	}
	skip := int(start - r.startOffset)
	readAt := (r.start + skip) % len(r.data)
	first := min(length, len(r.data)-readAt)
	copy(result, r.data[readAt:readAt+first])
	copy(result[first:], r.data[:length-first])
	return result, start, end, truncated, true
}

func (r *byteRing) clear() {
	clear(r.data)
	r.start = 0
	r.length = 0
	r.startOffset = r.endOffset
}
