package gatewayservice

import (
	"bytes"
	"encoding/xml"
	"strings"
)

func render(c Config, name string) ([]byte, error) {
	if c.Platform == Linux {
		arguments := []string{c.Executable, "gateway", "serve", "--profile", c.ProfileDirectory}
		for i := range arguments {
			arguments[i] = unitQuote(arguments[i], true)
		}
		unit := "# Managed by delegate gateway\n[Unit]\nDescription=Delegate Gateway connector\n[Service]\nType=exec\nExitType=main\nDelegate=yes\nKillMode=process\nSendSIGKILL=no\nTimeoutStopSec=infinity\nRestart=no\nUMask=0077\nExecStart=" + strings.Join(arguments, " ") + "\nEnvironment=" + unitQuote("PATH="+c.Path, false) + "\n[Install]\nWantedBy=default.target\n"
		return []byte(unit), nil
	}
	var b bytes.Buffer
	b.WriteString(xml.Header)
	b.WriteString("<plist version=\"1.0\"><dict><key>Label</key>")
	writeXMLString(&b, name)
	b.WriteString("<key>ProgramArguments</key><array>")
	for _, value := range []string{c.Executable, "gateway", "serve", "--profile", c.ProfileDirectory} {
		writeXMLString(&b, value)
	}
	b.WriteString("</array><key>EnvironmentVariables</key><dict><key>PATH</key>")
	writeXMLString(&b, c.Path)
	b.WriteString("</dict><key>AbandonProcessGroup</key><true/><key>ExitTimeOut</key><integer>0</integer><key>KeepAlive</key><false/><key>RunAtLoad</key><true/></dict></plist>\n")
	return b.Bytes(), nil
}

func unitQuote(value string, command bool) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	value = strings.ReplaceAll(value, "%", "%%")
	if command {
		value = strings.ReplaceAll(value, "$", "$$")
	}
	return "\"" + value + "\""
}

func writeXMLString(b *bytes.Buffer, value string) {
	b.WriteString("<string>")
	// bytes.Buffer writes cannot fail; EscapeText's writer has that guarantee.
	if err := xml.EscapeText(b, []byte(value)); err != nil {
		panic(err)
	}
	b.WriteString("</string>")
}
