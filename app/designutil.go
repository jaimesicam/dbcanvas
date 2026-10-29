package main

import "encoding/json"

// nodeLabel is a node's label in the design, or its id when the design has no such node.
func nodeLabel(doc designDoc, nodeID string) string {
	for _, n := range doc.Nodes {
		if n.ID == nodeID {
			return n.Label
		}
	}
	return nodeID
}

// frameByID is the design's frame with that id, or the zero frame.
func frameByID(doc designDoc, id string) designFrame {
	for _, f := range doc.Frames {
		if f.ID == id {
			return f
		}
	}
	return designFrame{}
}

// mustJSON marshals v, for values that always marshal.
func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
