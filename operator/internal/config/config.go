// Package config holds operator-wide defaults supplied by flags.
package config

// Defaults apply when a KubeCostHPA does not override them.
type Defaults struct {
	CPUPricePerCoreHour float64
	MemPricePerGiBHour  float64
}
