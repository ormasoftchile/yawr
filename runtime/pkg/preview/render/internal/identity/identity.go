package identity

import "github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"

func Node(node *graphdoc.Node) string {
	if node.QualifiedID != "" {
		return node.QualifiedID
	}
	return node.ID
}

func NodeFrame(node *graphdoc.Node) string {
	if node.QualifiedFrameID != "" {
		return node.QualifiedFrameID
	}
	return node.FrameID
}

func NodeGroup(node *graphdoc.Node) string {
	if node.QualifiedGroupID != "" {
		return node.QualifiedGroupID
	}
	return node.GroupID
}

func Frame(frame *graphdoc.Frame) string {
	if frame.QualifiedID != "" {
		return frame.QualifiedID
	}
	return frame.ID
}

func FrameParentNode(frame *graphdoc.Frame) string {
	if frame.QualifiedParentIncludeNodeID != "" {
		return frame.QualifiedParentIncludeNodeID
	}
	return frame.ParentIncludeNodeID
}

func Group(group *graphdoc.Group) string {
	if group.QualifiedID != "" {
		return group.QualifiedID
	}
	return group.ID
}

func GroupParentNode(group *graphdoc.Group) string {
	if group.QualifiedParentNodeID != "" {
		return group.QualifiedParentNodeID
	}
	return group.ParentNodeID
}
