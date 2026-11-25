package biz

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/jucode/jucode/internal/data/ent"
)

// A definition reaches the console through two builders: the shipped copy and a
// database row. Every field the shipped copy can express has to survive both
// ways — a flag that only one path fills makes the console ask for the wrong
// thing (that is exactly how a definition declaring its own login handshake
// still offered a "paste your token" box).
//
// The row below is built from the shipped definition itself, so this comparison
// fails as soon as a field is filled on one side only. Differences that are
// intended are listed here with their reason, so a new one cannot hide among
// them.
var knownViewDifferences = map[string]string{
	"Source":    "builtin vs database: tells the console where the definition came from",
	"Version":   "rows carry a revision, the shipped copy is version 0",
	"UpdatedAt": "a shipped definition has no edit timestamp",
}

func TestShippedDefinitionsSurviveBothViewBuilders(t *testing.T) {
	builtins := builtinDefinitions()
	if len(builtins) == 0 {
		t.Fatal("no shipped definitions to check")
	}
	for _, builtin := range builtins {
		t.Run(builtin.DefinitionID, func(t *testing.T) {
			fromCode := viewFromBuiltin(builtin)
			row := &ent.ProviderDefinition{
				DefinitionID:    builtin.DefinitionID,
				Name:            builtin.Name,
				Vendor:          builtin.Vendor,
				BaseURL:         builtin.BaseURL,
				APIFormat:       builtin.APIFormat,
				NativeProtocols: builtin.NativeProtocols,
				DefaultProtocol: builtin.DefaultProtocol,
				EndpointPaths:   builtin.EndpointPaths,
				HeaderRules:     builtin.HeaderRules,
				CredentialLabel: builtin.CredentialLabel,
				CredentialHint:  builtin.CredentialHint,
				ModelsJSON:      builtin.ModelsJSON,
				ModelsPath:      builtin.ModelsPath,
				Note:            builtin.Note,
				Enabled:         true,
				Builtin:         true,
			}
			fromRow := (&ProviderDefinitionSvc{}).viewFromRow(row)

			code := reflect.ValueOf(fromCode)
			rowView := reflect.ValueOf(fromRow)
			typ := code.Type()
			for i := 0; i < typ.NumField(); i++ {
				field := typ.Field(i)
				if _, intended := knownViewDifferences[field.Name]; intended {
					continue
				}
				left := code.Field(i).Interface()
				right := rowView.Field(i).Interface()
				if !reflect.DeepEqual(left, right) {
					t.Errorf("field %s differs between the shipped copy and a database row: builtin=%s row=%s",
						field.Name, describeViewValue(left), describeViewValue(right))
				}
			}
		})
	}
}

func describeViewValue(value any) string {
	return fmt.Sprintf("%v", value)
}
