package hpack

type HPackDecoder struct {
	indexTable *indexTable
}

func Decoder() *HPackDecoder {
	return &HPackDecoder{
		indexTable: NewIndexTable(),
	}
}

func decInt(bs *[]byte, prefix int) int {
	mask := (1 << prefix) - 1
	i := int((*bs)[0]) & mask
	if i < mask {
		*bs = (*bs)[1:]
		return i
	}

	m := 0
	for {
		*bs = (*bs)[1:]
		oct := (*bs)[0]
		i += int(oct&127) << m
		m += 7
		if oct&128 != 128 {
			break
		}
	}

	return i
}

func readStringLiteral(bs *[]byte) (string, error) {
	huffman := (*bs)[0]&0x80 != 0 // huffman
	n := decInt(bs, 7)
	dec := (*bs)[:n]
	var str string
	var err error
	if huffman {
		str, err = HuffmanDecoder(dec)
		if err != nil {
			return "", err
		}
	} else {
		str = string(dec)
	}

	*bs = (*bs)[n:]
	return str, nil
}

func (h *HPackDecoder) readHeaderFieldInternal(bs *[]byte, idx int) (Header, error) {
	if idx > 0 {
		header, err := h.indexTable.Get(idx)
		if err != nil {
			return Header{}, err
		}
		val, err := readStringLiteral(bs)
		if err != nil {
			return Header{}, err
		}
		return Header{
			Name:  header.Name,
			Value: val,
		}, nil
	} else {
		name, err := readStringLiteral(bs)
		if err != nil {
			return Header{}, err
		}
		val, err := readStringLiteral(bs)
		if err != nil {
			return Header{}, err
		}
		return Header{
			Name:  name,
			Value: val,
		}, nil
	}
}

func (h *HPackDecoder) Decode(bs []byte) ([]Header, error) {
	headers := []Header{}
	for len(bs) > 0 {
		field := bs[0]

		indexedHeader := (field & 0x80) != 0
		incrementedIndexed := (field & 0xc0) == 0x40

		woIndexing := (field & 0xf0) == 0
		neverIndexing := (field & 0xf0) == 0x10
		sizeUpdate := (field & 0xe0) == 0x20

		if indexedHeader {
			idx := decInt(&bs, 7)
			header, err := h.indexTable.Get(idx)
			if err != nil {
				return nil, err
			}

			headers = append(headers, header)
		} else if incrementedIndexed {
			header, err := h.readHeaderFieldInternal(&bs, decInt(&bs, 6))
			if err != nil {
				return nil, err
			}
			h.indexTable.Add(header)
			headers = append(headers, header)
		} else if woIndexing || neverIndexing {
			header, err := h.readHeaderFieldInternal(&bs, decInt(&bs, 4))
			if err != nil {
				return nil, err
			}
			header.neverIndexed = neverIndexing
			headers = append(headers, header)
		} else if sizeUpdate {
			// TODO: update dynamic table size
			func() {}()
		}
	}
	return headers, nil
}
