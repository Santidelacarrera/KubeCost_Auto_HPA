// Command simbench prints the synthetic HPA-vs-KubeCostAutoscaler comparison as Markdown. It is
// deterministic: the same binary always prints the same numbers. See internal/sim for what is
// (and is not) modelled; nothing here is a measurement of a real cluster.
package main

import (
	"fmt"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/sim"
)

func main() { fmt.Print(sim.Markdown()) }
