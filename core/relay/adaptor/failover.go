package adaptor

import "github.com/labring/aiproxy/core/common/failover"

// WithFailover preserves the public error envelope while carrying private evidence.
func WithFailover(err Error, failure failover.Failure) Error {
	if err == nil {
		return nil
	}
	return &failoverError{wrappedError: err, failure: failure}
}

type wrappedError = Error
type failoverError struct {
	wrappedError
	failure failover.Failure
}

func (e *failoverError) FailoverFailure() failover.Failure { return e.failure }
func (e *failoverError) Unwrap() error                     { return e.wrappedError }

func (e *failoverError) ErrorCode() any {
	if provider, ok := e.wrappedError.(ErrorCodeProvider); ok {
		return provider.ErrorCode()
	}
	return nil
}
