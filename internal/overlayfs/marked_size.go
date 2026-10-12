package overlayfs

import "io"

// markedSize mirrors the document layout using sizes and fresh final bytes.
// Metadata requests must not allocate and read the whole merged document.
func (n *node) markedSize(name string, parts []contribution, rule *MarkerRule) (int64, error) {
	var size int64
	for i, part := range parts {
		if part.info == nil {
			continue // The empty project section has no source copy.
		}
		length := part.info.Size()
		size += length
		if length > 0 {
			var tail [1]byte
			count, err := n.view.sniffs.read(n.view.layers[part.index].root, sniffKey{layer: part.index, name: name}, part.info, tail[:], length-1)
			if err != nil {
				return 0, err
			}
			if count != 1 {
				return 0, io.ErrUnexpectedEOF
			}
			if tail[0] != '\n' {
				size++
			}
		}
		if i == 0 {
			continue
		}
		start, end := n.view.sectionMarkers(name, part.index, rule)
		size += int64(len(start) + len(end) + 2)
	}
	return size, nil
}
