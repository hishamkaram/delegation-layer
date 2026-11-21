package app

import "github.com/hishamkaram/delegation-layer/internal/task"

// connectorOutputPolicy is invocation-local presentation state. Only a
// successfully validated Origin load may select the binding projection.
type connectorOutputPolicy uint8

const (
	connectorOutputLegacy connectorOutputPolicy = iota
	connectorOutputBinding
)

func (policy connectorOutputPolicy) withOrigin(origin task.GatewayOrigin) connectorOutputPolicy {
	if origin.Binding != "" {
		return connectorOutputBinding
	}
	return policy
}

type jsonOutputPolicy struct {
	limit     int
	connector connectorOutputPolicy
}

func commandJSONOutputPolicy(arguments Arguments, result commandResult) jsonOutputPolicy {
	limit := ordinaryJSONResponseLimit(result.response)
	if hasConnectorGuards(arguments.Connector) || result.response.Connector != nil {
		limit = 4 * task.MaxControlRecordSize
	}
	return jsonOutputPolicy{limit: limit, connector: result.connectorOutput}
}

func ordinaryJSONResponseLimit(response Response) int {
	if response.Command == "schedule" && response.Schedule != nil && response.Schedule.Request != nil {
		return 2 * task.MaxControlRecordSize
	}
	return MaxJSONResponseBytes
}

// Binding calls use the same public DTO with the control consumer's fields.
// Every changed nested value is copied; canonical evidence stays read-only.
func projectConnectorOutput(response Response, policy connectorOutputPolicy) Response {
	if policy != connectorOutputBinding {
		return response
	}
	projection := Response{
		SchemaVersion: response.SchemaVersion, Command: response.Command, RootID: response.RootID, TaskID: response.TaskID,
		TaskRecord: response.TaskRecord, Status: response.Status, Admission: response.Admission, Liveness: response.Liveness, Publication: response.Publication,
		Connector: response.Connector, ExecutionAssurance: response.ExecutionAssurance, Outcome: response.Outcome, Payload: response.Payload,
		ResultRange: response.ResultRange, Stop: response.Stop, Failure: response.Failure, Error: response.Error, ErrorTruncated: response.ErrorTruncated,
	}
	if response.Connector != nil {
		connector := *response.Connector
		connector.Features = uniqueConnectorFeatures(response.Connector.Features)
		projection.Connector = &connector
	}
	if response.Failure != nil {
		failure := *response.Failure
		failure.RecoveryCommand = ""
		projection.Failure = &failure
	}
	return projection
}

func uniqueConnectorFeatures(features []string) []string {
	if features == nil {
		return nil
	}
	unique := make([]string, 0)
	seen := make(map[string]struct{})
	for _, feature := range features {
		if _, exists := seen[feature]; exists {
			continue
		}
		seen[feature] = struct{}{}
		unique = append(unique, feature)
	}
	return unique
}
