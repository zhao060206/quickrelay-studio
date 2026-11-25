// Package version 集中定义 QuickRelay Studio 的版本号与上游 GitHub 仓库标识。
package version

const (
	// Current 为当前发行版本号。
	Current = "2.2.0"
	// RepoOwner 为官方 GitHub 仓库所有者。
	RepoOwner = "zhao060206"
	// RepoName 为官方 GitHub 仓库名称。
	RepoName = "quickrelay-studio"
	// DefaultRepo 为默认仓库路径（owner/repo）。
	DefaultRepo = RepoOwner + "/" + RepoName
)
