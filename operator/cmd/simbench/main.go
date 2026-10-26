// Command simbench prints the synthetic HPA-vs-KubeCostAutoscaler comparison as
// Markdown tables. It is deterministic: the same binary always prints the same
// numbers. See internal/sim for what is (and is not) modelled.
package main

import (
	"fmt"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/sim"
)

func main() {
	w := sim.DefaultWorkload()
	fmt.Printf("Workload: %.0f rps per ready pod, %s startup, $%.2f per replica-hour, replicas %d-%d, warm start\n\n",
		w.PerPodRPS, w.StartupDelay, w.PricePerHour, w.MinReplicas, w.MaxReplicas)
	for _, sc := range sim.Scenarios() {
		fmt.Printf("### %s (%s)\n\n", sc.Name, sc.Duration)
		fmt.Println("| Scaler | Cost USD | Avg replicas | Max replicas | Scale actions | Reversals | Unmet demand % | Saturated s | >90% util s | Longest saturation s |")
		fmt.Println("|---|---|---|---|---|---|---|---|---|---|")
		for _, s := range sim.Scalers(w) {
			r := sim.Run(w, sc, s)
			fmt.Printf("| %s | %.2f | %.1f | %d | %d | %d | %.2f | %.0f | %.0f | %.0f |\n",
				r.Scaler, r.CostUSD, r.AvgReplicas, r.MaxReplicas, r.ScaleActions, r.Reversals, r.UnmetDemandPct, r.SaturatedSeconds, r.AboveCeilingSecs, r.LongestSatSecs)
		}
		fmt.Println()
	}
}
