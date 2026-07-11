package main

func (s *Store) listAgentsForTelemetry(userID int64, projectID string) ([]Agent, error) {
	if projectID != "" {
		return s.ListAgentsInProject(projectID)
	}
	return s.ListAgents(userID, "")
}
