// Package models is the live model object of an agent host: every provider
// and model, their clients and their health (the Catalog, one per process),
// and which models serve which purpose in what order. A turn pins a Chain
// and calls it; the chain skips parked and disabled models, falls back on
// failure and records each model's health.
//
// The package never imports a host, and logs only through the logger it is
// given (WithLogger).
package models
