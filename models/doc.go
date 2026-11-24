// Package models is the live model object of an agent host: every provider
// and model, their clients and their health (the Catalog, one per process),
// and which models each agent uses for which purpose and in what order (a
// Set per agent). A turn pins a Chain from a Set and calls it; the chain
// skips parked and disabled models, falls back on failure and records each
// model's health.
//
// The package never imports a host, and logs only through the logger it is
// given (WithLogger).
package models
