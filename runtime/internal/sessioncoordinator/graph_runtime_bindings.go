package sessioncoordinator

import "github.com/ormasoftchile/yawr/runtime/pkg/preview/render/graphjson"

func graphRuntimeNodeID(node graphjson.Node) string {
	if id := graphDataString(node.Data, "runtime_node_id"); id != "" {
		return id
	}
	return node.ID
}

func appendGraphRuntimeBindings(document graphjson.Document, bindings *[]ExecutionGraphBinding) {
	for _, node := range document.Nodes {
		*bindings = append(*bindings, ExecutionGraphBinding{NodeID: node.ID, QualifiedNodeID: graphRuntimeNodeID(node)})
	}
}
