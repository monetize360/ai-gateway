package asyncjob

import "fmt"

var (
	ErrNotFound    = fmt.Errorf("job not found")
	ErrJobInternal = fmt.Errorf("internal job store error")
)
