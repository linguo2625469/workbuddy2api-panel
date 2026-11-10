#!/bin/bash
# 构建 WorkBuddy2API 菜单栏 app（产出可直接双击运行的 .app；加 --dmg 额外生成分发用 DMG）
#
# 用法：
#   bash macapp/build.sh                # 通用二进制（arm64 + x86_64）
#   bash macapp/build.sh --native       # 仅本机架构，构建更快（开发调试用）
#   bash macapp/build.sh --dmg          # 通用二进制 + 生成 macapp/dist/*.dmg
#   APP_VERSION=1.1.0 bash macapp/build.sh --dmg
set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
APP="$DIR/WorkBuddy2API.app"
EXEC="WorkBuddy2API"
VERSION="${APP_VERSION:-1.0.0}"
UNIVERSAL=1
MAKE_DMG=0

for arg in "$@"; do
  case "$arg" in
    --native) UNIVERSAL=0 ;;
    --dmg)    MAKE_DMG=1 ;;
    *) echo "未知参数：${arg}（支持 --native / --dmg）"; exit 1 ;;
  esac
done

rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
cp "$DIR/Info.plist" "$APP/Contents/Info.plist"
/usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString $VERSION" "$APP/Contents/Info.plist"
if [ -f "$DIR/Resources/AppIcon.icns" ]; then
  cp "$DIR/Resources/AppIcon.icns" "$APP/Contents/Resources/AppIcon.icns"
fi

FRAMEWORKS=(-framework SwiftUI -framework AppKit -framework WebKit -framework UserNotifications -framework Network)
SOURCES=("$DIR/Sources/"*.swift)

if [ "$UNIVERSAL" -eq 1 ]; then
  TMP="$(mktemp -d)"
  trap 'rm -rf "$TMP"' EXIT
  swiftc -O -swift-version 5 -parse-as-library -target arm64-apple-macos15.0 \
    "${FRAMEWORKS[@]}" -o "$TMP/arm64" "${SOURCES[@]}"
  swiftc -O -swift-version 5 -parse-as-library -target x86_64-apple-macos15.0 \
    "${FRAMEWORKS[@]}" -o "$TMP/x86_64" "${SOURCES[@]}"
  lipo -create "$TMP/arm64" "$TMP/x86_64" -output "$APP/Contents/MacOS/$EXEC"
else
  swiftc -O -swift-version 5 -parse-as-library \
    "${FRAMEWORKS[@]}" -o "$APP/Contents/MacOS/$EXEC" "${SOURCES[@]}"
fi

# ad-hoc 签名：保证通知与 WKWebView 正常（未做 Developer ID 公证，首次打开需右键 → 打开）
codesign --force --deep --sign - "$APP" >/dev/null 2>&1 || true

if [ "$MAKE_DMG" -eq 1 ]; then
  STAGE="$(mktemp -d)"
  cp -R "$APP" "$STAGE/"
  ln -s /Applications "$STAGE/Applications"   # 拖拽安装的经典布局
  mkdir -p "$DIR/dist"
  ARCH_TAG=$([ "$UNIVERSAL" -eq 1 ] && echo universal || echo native)
  DMG="$DIR/dist/WorkBuddy2API-$VERSION-$ARCH_TAG.dmg"
  rm -f "$DMG"
  hdiutil create -volname "WorkBuddy2API" -srcfolder "$STAGE" -ov -format UDZO "$DMG" >/dev/null
  rm -rf "$STAGE"
  echo "已生成 DMG：${DMG}"
fi

echo "构建完成：${APP}（版本 ${VERSION}）"
