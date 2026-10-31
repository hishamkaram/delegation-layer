package recurrence

import (
	"errors"
	"strings"
	"testing"
)

func TestGrammarRejectsExtensionsAndEmptyListElements(t *testing.T) {
	for _, expression := range []string{
		"", "@daily", "@every 1m", "0 * * * * *", "0 0 * * * 2027",
		"TZ=UTC 0 0 * * *", "CRON_TZ=UTC 0 0 * * *", "0 0 ? * MON",
		"0 0 L * *", "0 0 15W * *", "0 0 * * MON#2", "0 0 * * 5L",
		",0 0 * * *", "0, 0 * * *", "0,,1 0 * * *", "0 0 , * *",
		"0 0 *-2 * *", "0 0 * * *\n", strings.Repeat("0", 257) + " * * * *",
	} {
		if err := validateGrammar(expression); !errors.Is(err, ErrInvalidExpression) {
			t.Fatalf("expression %q accepted: %v", expression, err)
		}
	}
	for _, expression := range []string{"0 0 * * WED", "0,30 1-5/2 * JAN,MAR MON-FRI", "5/10 */2 */1 * SUN", "* * * * *"} {
		if err := validateGrammar(expression); err != nil {
			t.Fatalf("ordinary grammar %q rejected: %v", expression, err)
		}
	}
}

func TestWarningsAreBoundedPolicyAdvisories(t *testing.T) {
	for _, zone := range []string{"UTC", "Etc/UTC", "GMT", "Etc/GMT+5"} {
		warnings := Warnings("0 0 29 2 *", zone)
		if len(warnings) != 1 || warnings[0].Code != "calendar_omission" || !strings.Contains(warnings[0].Message, "leap years") {
			t.Fatalf("fixed zone %q warnings: %+v", zone, warnings)
		}
	}
	warnings := Warnings("30 2 * * *", "America/New_York")
	if len(warnings) != 2 || warnings[1].Code != "timezone_omission" {
		t.Fatalf("zone policy advisory missing: %+v", warnings)
	}
	warnings[0].Message = "caller mutation"
	if fresh := Warnings("30 2 * * *", "America/New_York"); fresh[0].Message == "caller mutation" {
		t.Fatal("warnings share mutable output")
	}
	if Warnings("@daily", "UTC") != nil || Warnings("* * * * *", "Local") != nil {
		t.Fatal("invalid definition advertised warnings")
	}
}
