package vault

// SetPublishHooksForTest observes the final preflight and rename from an external-package fixture.
// Tests using these hooks must not run in parallel.
func SetPublishHooksForTest(before func(), after func(string)) func() {
	oldBefore, oldAfter := beforeReplace, afterRename
	beforeReplace, afterRename = before, after
	return func() { beforeReplace, afterRename = oldBefore, oldAfter }
}
