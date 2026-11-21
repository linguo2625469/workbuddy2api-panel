#!/usr/bin/env bash
# 构建 Android 签到 App：先 gomobile bind 生成 AAR，再 Gradle 打包 debug APK。
set -euo pipefail
cd "$(dirname "$0")/.."

command -v go >/dev/null || { echo "go not found in PATH" >&2; exit 1; }
if ! grep -q 'golang.org/x/mobile' go.mod; then
    echo "go.mod 缺少 golang.org/x/mobile（gomobile bind 依赖）。" >&2
    echo "修复：go get golang.org/x/mobile@latest 后重新运行本脚本。" >&2
    exit 1
fi
# Git Bash 的 go env GOPATH 返回 Windows 路径（C:\...），需转成 POSIX 形式才能加入 PATH。
gopath_bin="$(go env GOPATH)/bin"
if command -v cygpath >/dev/null 2>&1; then
    gopath_bin="$(cygpath -u "$(go env GOPATH)")/bin"
fi
export PATH="$PATH:$gopath_bin"

: "${ANDROID_HOME:=D:/android/Sdk}"
export ANDROID_HOME
if [ -z "${ANDROID_NDK_HOME:-}" ]; then
    ndk_dir="$(ls -d "$ANDROID_HOME"/ndk/* 2>/dev/null | sort -V | tail -1 || true)"
    if [ -z "$ndk_dir" ]; then
        echo "NDK not found under $ANDROID_HOME/ndk" >&2
        exit 1
    fi
    export ANDROID_NDK_HOME="$ndk_dir"
fi

command -v gomobile >/dev/null || {
    echo "gomobile not found. Install: go install golang.org/x/mobile/cmd/gomobile@latest" >&2
    exit 1
}

# 刷新 gomobile 的绑定工具链；重复执行安全（幂等）。
gomobile init

mkdir -p android/app/libs
# Go 侧是中文注释，Windows 默认 GBK 编码会让生成的 Java 源码编译失败。
JAVA_TOOL_OPTIONS="${JAVA_TOOL_OPTIONS:+$JAVA_TOOL_OPTIONS }-Dfile.encoding=UTF-8" \
    gomobile bind -target=android/arm64,android/arm -androidapi 26 \
    -javapkg=com.wb2api.engine \
    -o android/app/libs/wbcheckin.aar ./mobile

cd android
./gradlew assembleDebug
echo "APK: android/app/build/outputs/apk/debug/app-debug.apk"
