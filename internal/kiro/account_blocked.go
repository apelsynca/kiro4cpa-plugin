package kiro

// Keep the exact legacy warning so refresh can remove it without touching user notes.
// Translation: Kiro denied this account (HTTP 403 User is not authorized).
// It may be blocked; re-enable it manually after confirming the block was lifted.
const (
	blockedNote = "Kiro \u62d2\u7edd\u4e86\u8be5\u8d26\u53f7\uff08HTTP 403 User is not authorized\uff09" +
		"\uff0c\u53ef\u80fd\u5df2\u88ab\u5c01\u7981\uff1b\u786e\u8ba4\u89e3\u5c01\u540e\u8bf7\u624b\u52a8\u542f\u7528"
	blockedAtKey = "kiro4cpa_blocked_at"
)
