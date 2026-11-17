package main

import (
	"database/sql"
	"encoding/json"
	"time"
)

func runtimeOperationTerminal(status string) bool {
	return status == "succeeded" || status == "failed" || status == "cancelled"
}
func runtimeOperationEvent(op runtimeOperation, name, project, topic string) (AppEvent, string) {
	data, _ := json.Marshal(op)
	return AppEvent{App: name, ProjectID: project, InstallID: op.InstallID, Topic: topic, Data: data, Time: time.Now().UTC()}, "runtime-" + op.ID + "-" + runtimeHash([]byte(op.UpdatedAt+":"+topic+":"+project))
}
func (s *Server) queueRuntimeOperationEventTx(tx *sql.Tx, op runtimeOperation, name, project, topic string) error {
	ev, key := runtimeOperationEvent(op, name, project, topic)
	_, err := s.queueAppSubscriptionsTx(tx, ev, key)
	return err
}
