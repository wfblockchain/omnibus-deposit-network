// Package iso reads and writes the ISO 20022 messages a corporate client
// exchanges with its bank, using moov-io/iso20022's message types:
//
//   - pain.001.001.10 customer credit transfer initiation, in
//   - pain.002.001.11 customer payment status report, out
//   - camt.053.001.08 bank-to-customer statement, out
//
// moov-io's types come straight from the XSDs, but two things they marshal
// are not schema-valid: an XSD choice is a struct whose unused branch is
// written as an empty element, and a zero ISODate is written as 0001-01-01.
// Every document written here goes through clean, which drops both, and the
// tests read each one back with moov-io's parser.
package iso

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/moov-io/iso20022/pkg/document"
)

// cents converts an ISO amount to cents, refusing fractions of a cent.
func cents(v float64) (int64, error) {
	c := math.Round(v * 100)
	if math.Abs(c-v*100) > 1e-6 {
		return 0, fmt.Errorf("amount %v has more than two decimals", v)
	}
	return int64(c), nil
}

// dollars converts cents to an ISO amount.
func dollars(c int64) float64 { return float64(c) / 100 }

// marshal renders a moov document as XML and cleans it.
func marshal(doc document.Iso20022Document) ([]byte, error) {
	raw, err := xml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	out, err := clean(raw)
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), out...), nil
}

type node struct {
	start    xml.StartElement
	children []*node
	text     string
}

// clean drops elements that carry nothing: no attributes other than a
// namespace, no text and no remaining children. A zero date counts as
// nothing. The result is indented.
func clean(in []byte) ([]byte, error) {
	d := xml.NewDecoder(bytes.NewReader(in))
	root := &node{}
	stack := []*node{root}
	for {
		tok, err := d.RawToken()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		top := stack[len(stack)-1]
		switch t := tok.(type) {
		case xml.StartElement:
			n := &node{start: t.Copy()}
			top.children = append(top.children, n)
			stack = append(stack, n)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			top.text += string(t)
		}
	}
	var b bytes.Buffer
	e := xml.NewEncoder(&b)
	e.Indent("", "  ")
	for _, n := range root.children {
		if prune(n) {
			continue
		}
		if err := emit(e, n); err != nil {
			return nil, err
		}
	}
	if err := e.Flush(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func prune(n *node) (empty bool) {
	kept := n.children[:0]
	for _, c := range n.children {
		if !prune(c) {
			kept = append(kept, c)
		}
	}
	n.children = kept
	text := strings.TrimSpace(n.text)
	if text == "0001-01-01" || strings.HasPrefix(text, "0001-01-01T") {
		text = ""
	}
	n.text = text
	return len(n.children) == 0 && text == "" && len(n.start.Attr) == 0
}

func emit(e *xml.Encoder, n *node) error {
	if err := e.EncodeToken(n.start); err != nil {
		return err
	}
	if n.text != "" {
		if err := e.EncodeToken(xml.CharData(n.text)); err != nil {
			return err
		}
	}
	for _, c := range n.children {
		if err := emit(e, c); err != nil {
			return err
		}
	}
	return e.EncodeToken(n.start.End())
}
