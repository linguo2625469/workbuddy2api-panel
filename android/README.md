# Android 签到 App

Android 客户端通过 gomobile 复用本仓库 `mobile` 包的签到逻辑，Kotlin 侧只负责界面与文件导入。

## 环境要求

- Go 1.23+
- gomobile（`go install golang.org/x/mobile/cmd/gomobile@latest`）
- JDK 17
- Android SDK Platform 35
- Android NDK

安装 gomobile 后，把 `$(go env GOPATH)/bin` 加入 `PATH`，否则构建脚本找不到 `gomobile`。

设置 SDK / NDK 路径：

```bash
export ANDROID_HOME=/path/to/android-sdk
export ANDROID_NDK_HOME=$ANDROID_HOME/ndk/<version>
```

## 构建

在仓库根目录执行：

```bash
scripts/build-android.sh
```

脚本先 `gomobile bind` 生成 AAR，再调用 Gradle 打包。产物：

```
android/app/build/outputs/apk/debug/app-debug.apk
```

仅构建 ARM ABI（`android/arm64`、`android/arm`），不支持 x86 / 模拟器。

> 警告：不要运行 `go mod tidy`。它会移除 `gomobile bind` 所需但未被代码直接引用的间接依赖 `golang.org/x/mobile`，导致构建失败。

## 使用

1. 导入面板 `auths/` 目录下的 `workbuddy-*.json` 账号文件。
2. 仅支持手动签到（单号签到或「全部签到」）。
3. 企业账号与国际版账号没有签到体系，会被自动跳过。
