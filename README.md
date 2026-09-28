# LanFileHub（内网文件共享）

LanFileHub 是一个面向 Windows 和 macOS 主电脑的轻量内网文件共享工具。主电脑运行一个独立程序，其他 Windows 或 macOS 电脑通过浏览器访问，不需要安装客户端。

## 下载

Windows 和 macOS 成品包请从 [GitHub Releases](https://github.com/2582767744/LanFileHub/releases) 下载。源码构建产物不会直接提交到 Git 历史。

## 项目文档

- [项目说明](项目说明.md)：产品定位、部署结构、安全边界、目录结构与维护方式
- [版本记录](CHANGELOG.md)：各版本新增功能、变更和限制
- [Windows 使用说明](使用说明.txt)
- [macOS 使用说明](使用说明-macOS.txt)

## 第一版功能

- 首次运行时选择主电脑上的本地共享文件夹
- 首次运行时设置管理员账号和密码
- 显示可供其他电脑访问的局域网地址
- 浏览共享目录和子目录
- 上传一个或多个文件
- 同名文件覆盖前进行确认
- 下载文件
- 新建文件夹
- 重命名文件或文件夹
- 删除文件或文件夹
- 主电脑可以随时更换共享目录或停止服务
- 单个上传文件最大 20GB，同时最多处理 4 个上传请求

## 数据位置

程序配置和有限大小的运行日志保存在：

```text
Windows: %APPDATA%\LanFileHub
macOS:   ~/Library/Application Support/LanFileHub
```

用户选择的共享文件夹不会被复制到程序数据目录，文件始终保存在用户指定的原始位置。

## 开发与构建

```powershell
go test ./...
go build -trimpath -buildvcs=false -ldflags "-s -w -H=windowsgui -X main.buildVersion=0.1.0" -o dist/LanFileHub-Windows-x64.exe .

$env:GOOS="darwin"; $env:GOARCH="arm64"; $env:CGO_ENABLED="0"
go build -trimpath -buildvcs=false -ldflags "-s -w -X main.buildVersion=0.1.0" -o dist/LanFileHub-macOS-arm64 .

$env:GOARCH="amd64"
go build -trimpath -buildvcs=false -ldflags "-s -w -X main.buildVersion=0.1.0" -o dist/LanFileHub-macOS-x64 .
```

生成的程序不依赖 Go、Node、Python 或数据库。macOS 内部使用版未签名，首次运行需要右键选择“打开”。

## 安全边界

- 只建议在可信任的家庭或办公局域网使用。
- 不要在路由器上将端口映射到互联网。
- 首次设置和更换共享目录只能在主电脑操作。
- 文件操作需要登录，并带有请求来源和会话校验。
- 登录连续失败会受到临时限制。
- 所有文件路径都会限制在用户选择的共享目录内。
- 当前版本使用局域网 HTTP；如需跨公网访问，应增加 HTTPS 或专用 VPN，不能直接暴露端口。

## 当前限制

- 当前 macOS 交付为未签名的内部使用版，未进行 Apple 公证。
- 只有一个共享账号，暂不支持多个用户和分目录权限。
- 删除是永久删除，尚未提供回收站和历史版本。
- 程序未进行商业代码签名，Windows SmartScreen 或 macOS Gatekeeper 可能在首次运行时提示未知发布者。
