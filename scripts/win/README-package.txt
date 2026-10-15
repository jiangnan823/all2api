all2api — Windows 安装说明
==========================

安装
  双击 install.bat（默认装到 Program Files，需要管理员确认），
  或在 PowerShell 里执行： .\install.ps1 -PerUser        （装到当前用户目录，无需管理员）
  可选参数： -Autostart（开机自启）  -Firewall（放行 7863 端口入站）

设置端口
  方式一：安装目录下的 config.json，改 "listen": ":7863" 为如 ":9000"，重启 all2api 生效。
  方式二：浏览器打开管理面板 http://localhost:7863 → 设置屏 → gateway 分组 → listen 字段，
          保存后按提示重启生效（面板保存走 POST /api/config，落盘到同一份 config.json）。

账号凭证
  把 workbuddy-*.json 放到安装目录的 auths\ 子目录（没有就新建），
  重启 all2api 后自动加载；token 过期会自动刷新并回写该文件。

卸载
  双击 uninstall.bat。
  默认保留你的数据（config.json、auths\ 凭证、data\ 状态）；
  确认不再需要时用： powershell -File uninstall.ps1 -Purge   （连数据一起删除）
