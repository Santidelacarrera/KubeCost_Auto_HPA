package gitops

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/recommend"
)

// editValues writes each changed container's requests (and limits, under the same rules as a manifest) into a Helm values file at
// the key the chart uses for that container's resources block. The key must already lead to a mapping in the file except for its
// last segment, which must be "resources": the values layout is the chart's contract and is never invented here. The current values
// in the file are checked against the recommendation (a stale one is refused).
func editValues(raw []byte, r recommend.Recommendation, keys map[string]string) ([]byte, error) {
	docs, err := decodeAll(raw)
	if err != nil {
		return nil, err
	}
	if len(docs) != 1 || top(docs[0]) == nil {
		return nil, errors.New("the values file must be one YAML mapping")
	}
	root := top(docs[0])
	ed := &edits{}
	for _, v := range r.Views() {
		if !isChange(v.CPU) && !isChange(v.Memory) {
			continue
		}
		key := keys[v.Container]
		if key == "" {
			return nil, fmt.Errorf("no values key for container %q: pass one, e.g. %s=resources", v.Container, v.Container)
		}
		parts := strings.Split(key, ".")
		if parts[len(parts)-1] != "resources" {
			return nil, fmt.Errorf("values key %q must end in .resources (the container's resources block)", key)
		}
		parent := root
		for _, p := range parts[:len(parts)-1] {
			if parent = get(parent, p); parent == nil || parent.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("values key %q: %q is not a mapping in the file (the layout is the chart's; it is not invented)", key, p)
			}
		}
		if err := setResources(parent, v, ed); err != nil {
			return nil, fmt.Errorf("container %s (values key %s): %w", v.Container, key, err)
		}
	}
	return render(raw, docs, ed)
}

// patchName is the file the Kustomize format owns, one per workload.
func patchName(r recommend.Recommendation) string {
	return fmt.Sprintf("rightsize-%s-%s.yaml", strings.ToLower(r.Kind), r.Workload)
}

// kustomizePatch returns a strategic-merge patch that sets the changed containers' resources, and the kustomization with that patch
// listed under `patches`. When base is given, the recommendation is first checked against it exactly as a manifest edit would be
// (stale values and limit rules); when an earlier patch of ours exists, it is updated in place and checked against its own values.
func kustomizePatch(r recommend.Recommendation, base, prior, kraw []byte, name string) ([]byte, []byte, error) {
	if base != nil {
		if _, err := edit(base, r); err != nil {
			return nil, nil, fmt.Errorf("checked against the base manifest: %w", err)
		}
	}
	var doc *yaml.Node
	if len(bytes.TrimSpace(prior)) > 0 {
		docs, err := decodeAll(prior)
		if err != nil || len(docs) != 1 || top(docs[0]) == nil {
			return nil, nil, fmt.Errorf("%s exists but is not a single patch document: refusing to overwrite it", name)
		}
		doc = docs[0]
		root := top(doc)
		if scalar(get(root, "kind")) != r.Kind || scalar(get(get(root, "metadata"), "name")) != r.Workload {
			return nil, nil, fmt.Errorf("%s patches another workload: refusing to overwrite it", name)
		}
	} else {
		doc = &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
		root := doc.Content[0]
		setPlain(root, "apiVersion", "apps/v1")
		setPlain(root, "kind", r.Kind)
		md := ensureMap(root, "metadata")
		setPlain(md, "name", r.Workload)
		if r.Namespace != "" {
			setPlain(md, "namespace", r.Namespace)
		}
	}
	root := top(doc)
	spec := ensureMap(ensureMap(ensureMap(root, "spec"), "template"), "spec")
	cs := get(spec, "containers")
	if cs == nil {
		cs = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		spec.Content = append(spec.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "containers"}, cs)
	}
	for _, v := range r.Views() {
		if !isChange(v.CPU) && !isChange(v.Memory) {
			continue
		}
		c := container(root, v.Container)
		if c == nil {
			// a new entry starts at the values the recommendation was computed for, so the same rules as a manifest edit apply
			c = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setPlain(c, "name", v.Container)
			res := ensureMap(c, "resources")
			req, lim := ensureMap(res, "requests"), ensureMap(res, "limits")
			for _, x := range []struct {
				n  string
				rr recommend.ResourceResult
				f  func(float64) string
			}{{"cpu", v.CPU, fmtCPU}, {"memory", v.Memory, fmtMem}} {
				if !isChange(x.rr) {
					continue
				}
				setScalar(req, x.n, x.f(x.rr.Current), nil)
				if x.rr.Limit > 0 {
					setScalar(lim, x.n, x.f(x.rr.Limit), nil)
				}
			}
			cs.Content = append(cs.Content, c)
		}
		if err := setResources(c, v, nil); err != nil {
			return nil, nil, fmt.Errorf("container %s: %w", v.Container, err)
		}
	}
	patch, err := encodeAll([]*yaml.Node{doc})
	if err != nil {
		return nil, nil, err
	}
	kdocs, err := decodeAll(kraw)
	if err != nil || len(kdocs) != 1 || top(kdocs[0]) == nil {
		return nil, nil, errors.New("the kustomization is not a single YAML mapping")
	}
	kroot := top(kdocs[0])
	if smp := get(kroot, "patchesStrategicMerge"); smp != nil {
		for _, e := range smp.Content {
			if e.Value == name {
				kout, err := encodeAll(kdocs)
				return patch, kout, err // already listed the deprecated way: leave it
			}
		}
	}
	ps := get(kroot, "patches")
	if ps == nil {
		ps = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		kroot.Content = append(kroot.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "patches"}, ps)
	}
	if ps.Kind != yaml.SequenceNode {
		return nil, nil, errors.New("the kustomization's `patches` is not a list")
	}
	listed := false
	for _, e := range ps.Content {
		listed = listed || scalar(get(e, "path")) == name
	}
	if !listed {
		e := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setPlain(e, "path", name)
		ps.Content = append(ps.Content, e)
	}
	kout, err := encodeAll(kdocs)
	return patch, kout, err
}

func fmtCPU(v float64) string {
	return resource.NewMilliQuantity(int64(math.Round(v*1000)), resource.DecimalSI).String()
}

func fmtMem(v float64) string {
	return resource.NewQuantity(int64(math.Round(v)), resource.BinarySI).String()
}

func setPlain(m *yaml.Node, key, val string) {
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: val})
}

func decodeAll(raw []byte) ([]*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var docs []*yaml.Node
	for {
		var n yaml.Node
		if err := dec.Decode(&n); err != nil {
			if errors.Is(err, io.EOF) {
				return docs, nil
			}
			return nil, fmt.Errorf("not valid YAML: %w", err)
		}
		docs = append(docs, &n)
	}
}

func encodeAll(docs []*yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	for _, d := range docs {
		if err := enc.Encode(d); err != nil {
			return nil, err
		}
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
