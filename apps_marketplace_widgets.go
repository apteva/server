package main

import sdk "github.com/apteva/app-sdk"

type marketplaceWidget struct {
	Component string `json:"component"`
	Label     string `json:"label,omitempty"`
}

func marketplaceWidgets(m *sdk.Manifest) []marketplaceWidget {
	out := []marketplaceWidget{}
	for _, c := range m.Provides.UIComponents {
		if c.Entry != "" && (containsString(c.Slots, "dashboard.home") || containsString(c.Slots, "project.page")) {
			out = append(out, marketplaceWidget{m.Name + ":" + c.Name, c.Label})
			if len(out) == 20 {
				break
			}
		}
	}
	return out
}

func marketplaceSuggestionScore(e RegistryEntry, terms []string) int {
	score := 0
	for _, term := range terms {
		score += marketplaceSearchScore(e, term, []string{term})
	}
	return score
}
