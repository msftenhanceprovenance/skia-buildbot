// Command-line application that parses a Promethues alerts file and produces a
// second alerts file that checks for missing data for each of the input
// alerts.
package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"strings"

	"github.com/golang/glog"
	"go.skia.org/infra/go/common"
)

// flags
var (
	in  = flag.String("in", "sys/alert.rules", "The Prometheus alerts to read as input.")
	out = flag.String("out", "sys/absent.rules", "The Prometheus alerts to write as output.")
)

const (
	RULE = `ALERT MissingData
  IF absent(%s)
  FOR 5m
  LABELS { category = "infra", severity = "critical" }
  ANNOTATIONS {
    abbr = "%s",
    description = "There is no data for the following alert: %s"
  }

`
)

func main() {
	common.Init()
	// Open input file (alert.rules)
	b, err := ioutil.ReadFile(*in)
	if err != nil {
		glog.Fatalf("Failed to open input file %q: %s", *in, err)
	}

	body := []string{
		"# DO NOT EDIT: This file is generated by running the 'absent' tool.\n\n",
	}
	replacer := strings.NewReplacer("<", "", ">", "", "=", "", "!", "")
	escaper := strings.NewReplacer("\"", "\\\"")
	lines := strings.Split(string(b), "\n")
	for _, line := range lines {
		// The IF lines look like:
		//
		//		IF prober{type="failure"} > 0
		//
		// Parse up each "IF" statement.
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "IF ") {
			continue
		}
		parts := strings.Split(line, " ")

		// Check that the second to last part is a valid comparison operator.
		if len(parts) < 4 {
			glog.Fatalf("Does not appear to be a valid IF statement: %q", line)
		}
		op := parts[len(parts)-2]
		if len(op) == 0 {
			glog.Fatalf("Missing a valid comparison operator in IF statement: %q", line)
		}
		empty := replacer.Replace(op)
		if empty != "" {
			glog.Fatalf("Not a valid comparison operator %q in IF statement %q:", op, line)
		}

		// We are interested in just the expression w/o the comparison, or the "IF".
		expression := strings.Join(parts[1:len(parts)-2], "")
		// Escape quotes in the expression.
		escaped := escaper.Replace(expression)
		// Create a new rule that checks if there is no data for the expression.
		body = append(body, fmt.Sprintf(RULE, expression, escaped, escaped))
	}
	// Write the output file.
	if err := ioutil.WriteFile(*out, []byte(strings.Join(body, "")), 0644); err != nil {
		glog.Fatalf("Failed to write output file %q: %s", *out, err)
	}
}
