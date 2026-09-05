package contentaudit

// ReviewFailure carries diagnostic codes without exposing an upstream response,
// URL, credential, or customer text through Error. Cause is retained for errors.Is.
type ReviewFailure struct {
	Code             string
	Cause            error
	StageLatenciesMS map[string]int64
}

func (e *ReviewFailure) Error() string {
	return "content audit model review: " + e.AuditReviewFailureCode()
}

func (e *ReviewFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *ReviewFailure) AuditReviewFailureCode() string {
	if e == nil || len(e.Code) == 0 || len(e.Code) > 64 {
		return "review_error"
	}
	for _, r := range e.Code {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return "review_error"
		}
	}
	return e.Code
}

func (e *ReviewFailure) AuditReviewStageLatenciesMS() map[string]int64 {
	if e == nil {
		return nil
	}
	copyStages := make(map[string]int64, len(e.StageLatenciesMS))
	for stage, millis := range e.StageLatenciesMS {
		copyStages[stage] = millis
	}
	return copyStages
}
