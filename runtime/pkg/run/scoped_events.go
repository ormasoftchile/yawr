package run

import "github.com/ormasoftchile/yawr/runtime/pkg/engine"

func scopedRunEventObserver(onEvent, onSubEvent func(engine.Event)) func(engine.Event) {
	if onSubEvent == nil {
		return onEvent
	}
	return func(event engine.Event) {
		stepID, _ := event.Payload["step_id"].(string)
		qualifiedID, _ := event.Payload["qualified_node_id"].(string)
		if stepID != "" && qualifiedID != "" && qualifiedID != engine.DebugNodeID(nil, stepID) {
			onSubEvent(event)
			return
		}
		if onEvent != nil {
			onEvent(event)
		}
	}
}
