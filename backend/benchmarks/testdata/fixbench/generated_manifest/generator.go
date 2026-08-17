package manifest

import "fmt"

// RenderCronJob renders the generated CronJob fixture.
func RenderCronJob(name string) string {
	return fmt.Sprintf(`apiVersion: batch/v1
kind: CronJob
metadata:
  name: %s
spec:
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: Never
`, name)
}
