package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/approval"
	"github.com/santidelacarrera/kubecost_auto_hpa/operator/internal/execute"
)

// keygen writes an approver's Ed25519 key pair: <out>.key (private, mode 0600, stays with the approver) and <out>.pub (goes into the
// policy's approverKeys under the printed key ID).
func keygenCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	var prefix string
	fs.StringVar(&prefix, "out", "", "path prefix for <out>.key and <out>.pub (required).")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if prefix == "" {
		return fmt.Errorf("--out is required")
	}
	for _, p := range []string{prefix + ".key", prefix + ".pub"} {
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("%s exists: refusing to overwrite a key", p)
		}
	}
	priv, pub, id, err := approval.GenerateKey(nil)
	if err != nil {
		return err
	}
	if err := os.WriteFile(prefix+".key", priv, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(prefix+".pub", pub, 0o644); err != nil {
		return err
	}
	return printJSON(out, map[string]any{"keyID": id, "private": prefix + ".key", "public": prefix + ".pub",
		"policy": fmt.Sprintf("requireSignedApproval: true\napproverKeys:\n  %s: |\n    <contents of %s.pub>", id, prefix)})
}

// audit-verify checks the hash chain of an audit file written with --audit-file.
func auditVerifyCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("audit-verify", flag.ContinueOnError)
	var file string
	fs.StringVar(&file, "audit-file", "", "JSONL audit file (required).")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if file == "" {
		return fmt.Errorf("--audit-file is required")
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	rep, err := execute.VerifyChain(f)
	if err != nil {
		return err
	}
	if err := printJSON(out, rep); err != nil {
		return err
	}
	if rep.FirstBreak != 0 {
		return fmt.Errorf("the audit chain is broken at line %d: %s", rep.FirstBreak, rep.Problem)
	}
	return nil
}
