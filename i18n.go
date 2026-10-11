package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// Lang indexes the message table below.
type Lang int

const (
	LangEN Lang = iota // default and fallback
	LangZH
)

type langCtxKey struct{}

// msg is a message key. Using a type rather than a bare string turns a typo
// into a compile error instead of a silently untranslated message.
type msg string

const (
	msgRepoFormat       msg = "repo.format"
	msgRepoBadChars     msg = "repo.badChars"
	msgRepoUnknownHost  msg = "repo.unknownHost"
	msgRepoTypeUnknown  msg = "repo.typeUnknown"
	msgRepoNotFound     msg = "repo.notFound"
	msgRepoRateLimited  msg = "repo.rateLimited"
	msgRepoRateLimitedB msg = "repo.rateLimitedBrief"
	msgRepoForbidden    msg = "repo.forbidden"
	msgRepoBadToken     msg = "repo.badToken"
	msgRepoRequestFail  msg = "repo.requestFailed"
	msgRepoDecodeFail   msg = "repo.decodeFailed"

	msgAssetRegexBad   msg = "regex.assetBad"
	msgVersionRegexBad msg = "regex.versionBad"
	msgNoVersions      msg = "resolve.noVersions"

	msgSkipDraft      msg = "skip.draft"
	msgSkipPrerelease msg = "skip.prerelease"
	msgSkipNoAssets   msg = "skip.noAssets"
	msgSkipNoMatch    msg = "skip.noMatch"
	msgSkipExternal   msg = "skip.external"
	msgSkipNoVersion  msg = "skip.noVersion"
	msgSkipDuplicate  msg = "skip.duplicate"

	msgAppNameRequired   msg = "app.nameRequired"
	msgBundleRequired    msg = "app.bundleRequired"
	msgBundleMalformed   msg = "app.bundleMalformed"
	msgAppNotFound       msg = "app.notFound"
	msgGroupNotFound     msg = "group.notFound"
	msgGroupNotFoundHint msg = "group.notFoundHint"
	msgGroupExists       msg = "group.exists"
	msgGroupNameExists   msg = "group.nameExists"
	msgGroupSlugEmpty    msg = "group.slugEmpty"
	msgGroupSlugLong     msg = "group.slugLong"
	msgGroupSlugReserved msg = "group.slugReserved"
	msgGroupIdentClash   msg = "group.identClash"
	msgGroupIdentTaken   msg = "group.identTaken"

	msgSourceNeedsJSON msg = "source.needsJson"
	msgSourceNoGroup   msg = "source.noGroup"
	msgBadRequest      msg = "request.badJson"
)

// Each entry holds the English text first and the Chinese text second, with
// identical fmt verbs in both.
var messages = map[msg][2]string{
	msgRepoFormat: {
		"Expected owner/repo, for example loopwerk/AltStore",
		"需要 owner/repo 形式，例如 loopwerk/AltStore",
	},
	msgRepoBadChars: {
		"Repository name cannot contain spaces or query parameters: %q",
		"仓库名不能包含空格或查询参数：%q",
	},
	msgRepoUnknownHost: {
		"Unsupported host %q — releases can only be read from %s",
		"不支持的站点 %q——目前只能从 %s 读取 release",
	},
	msgRepoTypeUnknown: {
		"Unknown repository type %q",
		"未知的仓库类型 %q",
	},
	msgRepoNotFound: {
		"Repository not found on %s, or it is private (private repos need a token in Repo settings)",
		"在 %s 上找不到这个仓库，或者是私有仓库（私有仓库需要在「仓库设置」里填 Token）",
	},
	msgRepoRateLimited: {
		"%s rate limit reached; resets in %s. Add a token or raise the cache time to avoid this",
		"%s 接口限流，%s 后恢复。填一个 Token 或调大缓存时间可以避免",
	},
	msgRepoRateLimitedB: {
		"%s rate limit reached. Try again later, or add a token",
		"%s 接口限流，请稍后再试或填写 Token",
	},
	msgRepoForbidden: {
		"%s refused the request: %s",
		"%s 拒绝访问：%s",
	},
	msgRepoBadToken: {
		"The token for %s is invalid or expired",
		"%s 的 Token 无效或已过期",
	},
	msgRepoRequestFail: {
		"Request to %s failed: %v",
		"请求 %s 失败：%v",
	},
	msgRepoDecodeFail: {
		"Could not parse the %s response: %v",
		"解析 %s 的响应失败：%v",
	},

	msgAssetRegexBad: {
		"Invalid asset regex: %v",
		"资源正则无效：%v",
	},
	msgVersionRegexBad: {
		"Invalid version regex: %v",
		"版本正则无效：%v",
	},
	msgNoVersions: {
		"No versions matched. Check that the regex matches the release asset filenames — it is a partial match against the filename, e.g. (?i)\\.ipa$",
		"没有匹配到任何版本。检查正则是否能匹配 release 附件的文件名——正则是对文件名做部分匹配，例如 (?i)\\.ipa$",
	},

	msgSkipDraft: {
		"draft",
		"草稿",
	},
	msgSkipPrerelease: {
		"pre-release excluded",
		"预发布版本已排除",
	},
	msgSkipNoAssets: {
		"this release has no assets",
		"该 release 没有附件",
	},
	msgSkipNoMatch: {
		"no filename matched the regex",
		"没有文件名匹配正则",
	},
	msgSkipExternal: {
		"the matching files are external links, which are excluded",
		"命中的是外链附件，已按设置排除",
	},
	msgSkipNoVersion: {
		"could not extract a version from the tag",
		"无法从 tag 提取版本号",
	},
	msgSkipDuplicate: {
		"version %s already seen",
		"版本号 %s 重复",
	},

	msgAppNameRequired: {
		"App name is required",
		"应用名称不能为空",
	},
	msgBundleRequired: {
		"Bundle ID is required",
		"Bundle ID 不能为空",
	},
	msgBundleMalformed: {
		"Bundle ID looks wrong; it should be something like com.example.app",
		"Bundle ID 格式不对，应类似 com.example.app",
	},
	msgAppNotFound: {
		"App not found",
		"应用不存在",
	},
	msgGroupNotFound: {
		"Group not found",
		"分组不存在",
	},
	msgGroupNotFoundHint: {
		"Group %q does not exist; create it under Groups first",
		"分组 %q 不存在，先在「分组管理」里创建",
	},
	msgGroupExists: {
		"Group %q already exists",
		"分组 %q 已存在",
	},
	msgGroupNameExists: {
		"A group named %q already exists",
		"已存在同名分组 %q",
	},
	msgGroupSlugEmpty: {
		"Group slug cannot be empty; use a-z, 0-9, - and _ only",
		"分组标识不能为空，只能用 a-z、0-9、- 和 _",
	},
	msgGroupSlugLong: {
		"Group slug cannot be longer than 64 characters",
		"分组标识不能超过 64 个字符",
	},
	msgGroupSlugReserved: {
		"%q is reserved — it always means every app",
		"%q 是保留名，换一个（它固定指向全部应用）",
	},
	msgGroupIdentClash: {
		"Identifier duplicates the source's own identifier; AltStore would treat the two as one source",
		"标识符和源本身的标识符重复了，AltStore 会把两个源当成同一个",
	},
	msgGroupIdentTaken: {
		"Identifier is already used by group %q",
		"标识符已被分组 %q 使用",
	},

	msgSourceNeedsJSON: {
		"Feed URLs must end in .json, for example /source/all.json",
		"订阅地址应以 .json 结尾，例如 /source/all.json",
	},
	msgSourceNoGroup: {
		"No such group: %s",
		"没有这个分组：%s",
	},
	msgBadRequest: {
		"Could not parse the request body: %v",
		"请求内容无法解析：%v",
	},
}

func withLang(ctx context.Context, l Lang) context.Context {
	return context.WithValue(ctx, langCtxKey{}, l)
}

// langFrom returns the language carried by ctx, defaulting to English.
func langFrom(ctx context.Context) Lang {
	if l, ok := ctx.Value(langCtxKey{}).(Lang); ok {
		return l
	}
	return LangEN
}

// langOf picks a language from Accept-Language. A missing or unrecognised
// header falls back to English, which is the product default.
func langOf(r *http.Request) Lang {
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		tag := strings.ToLower(strings.TrimSpace(part))
		if i := strings.IndexByte(tag, ';'); i >= 0 {
			tag = strings.TrimSpace(tag[:i])
		}
		switch {
		case tag == "":
			continue
		case strings.HasPrefix(tag, "zh"):
			return LangZH
		case strings.HasPrefix(tag, "en"):
			return LangEN
		}
	}
	return LangEN
}

// tr renders a message in the language carried by ctx. Unknown keys are
// returned as-is so a missed translation is visible rather than blank.
func tr(ctx context.Context, key msg, args ...any) string {
	entry, ok := messages[key]
	if !ok {
		return string(key)
	}
	s := entry[langFrom(ctx)]
	if len(args) == 0 {
		return s
	}
	return fmt.Sprintf(s, args...)
}

// trf builds a localized error, for the many call sites that return one.
func trf(ctx context.Context, key msg, args ...any) error {
	return fmt.Errorf("%s", tr(ctx, key, args...))
}
