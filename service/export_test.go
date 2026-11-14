package service

// Test-only access to internals for the external service_test package.

// SetTestHookMoveCopied installs f to run between a move's copy and its
// source delete; the returned func restores the previous hook.
func SetTestHookMoveCopied(f func()) func() {
	prev := testHookMoveCopied
	testHookMoveCopied = f
	return func() { testHookMoveCopied = prev }
}

// StripeForTest returns the key-lock stripe of (bucketID, key).
func StripeForTest(bucketID int64, key string) int { return stripeOf(bucketID, key) }

// RLockUploadForTest takes the shared (part upload) side of an upload lock.
func (s *Objects) RLockUploadForTest(uploadID string) func() { return s.rlockUpload(uploadID) }

// UploadLockCountForTest reports how many upload locks are allocated.
func (s *Objects) UploadLockCountForTest() int {
	s.uploadLocks.mu.Lock()
	defer s.uploadLocks.mu.Unlock()
	return len(s.uploadLocks.m)
}
