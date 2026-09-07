package main

import "context"

func integrationRequestContext(parents []context.Context) context.Context {
	if len(parents) > 0 && parents[0] != nil {
		return parents[0]
	}
	return context.Background()
}
