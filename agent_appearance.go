package main

import (
	"database/sql"
	"fmt"
	"strings"
)

const (
	defaultAgentIcon      = "robot"
	defaultAgentIconColor = "accent"
)

var agentIcons = map[string]bool{
	"robot": true, "assistant": true, "code": true, "research": true,
	"writer": true, "analyst": true, "support": true, "operations": true,
	"manager": true, "social": true, "design": true, "sales": true,
	"marketing": true, "finance": true, "security": true, "planner": true,
}

var agentIconColors = map[string]bool{
	"accent": true, "blue": true, "green": true,
	"purple": true, "pink": true, "teal": true,
}

func validAgentAppearance(icon, color string) bool {
	return agentIcons[icon] && agentIconColors[color]
}

func (s *Store) updateAgentIdentity(id int64, name, icon, color *string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if name != nil {
		if _, err := tx.Exec("UPDATE agents SET name=? WHERE id=?", *name, id); err != nil {
			return err
		}
	}
	if icon != nil || color != nil {
		currentIcon, currentColor := defaultAgentIcon, defaultAgentIconColor
		err := tx.QueryRow("SELECT icon, icon_color FROM agent_appearances WHERE agent_id=?", id).Scan(&currentIcon, &currentColor)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if icon != nil {
			currentIcon = *icon
		}
		if color != nil {
			currentColor = *color
		}
		if !validAgentAppearance(currentIcon, currentColor) {
			return fmt.Errorf("invalid agent appearance")
		}
		if _, err := tx.Exec(`INSERT INTO agent_appearances(agent_id, icon, icon_color) VALUES(?, ?, ?)
			ON CONFLICT(agent_id) DO UPDATE SET icon=excluded.icon, icon_color=excluded.icon_color`, id, currentIcon, currentColor); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) populateAgentAppearances(agents []Agent) error {
	if len(agents) == 0 {
		return nil
	}
	index := make(map[int64]int, len(agents))
	for i := range agents {
		agents[i].Icon = defaultAgentIcon
		agents[i].IconColor = defaultAgentIconColor
		index[agents[i].ID] = i
	}
	for start := 0; start < len(agents); start += 500 {
		end := min(start+500, len(agents))
		args := make([]any, end-start)
		for i := start; i < end; i++ {
			args[i-start] = agents[i].ID
		}
		query := "SELECT agent_id, icon, icon_color FROM agent_appearances WHERE agent_id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(args)), ",") + ")"
		rows, err := s.db.Query(query, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			var icon, color string
			if err := rows.Scan(&id, &icon, &color); err != nil {
				rows.Close()
				return err
			}
			if i, ok := index[id]; ok && validAgentAppearance(icon, color) {
				agents[i].Icon, agents[i].IconColor = icon, color
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	return nil
}

func (s *Store) populateAgentAppearance(agent *Agent) error {
	rows := []Agent{*agent}
	if err := s.populateAgentAppearances(rows); err != nil {
		return err
	}
	agent.Icon, agent.IconColor = rows[0].Icon, rows[0].IconColor
	return nil
}
