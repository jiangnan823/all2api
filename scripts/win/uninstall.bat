@echo off
rem Double-click wrapper: runs uninstall.ps1 next to this file.
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0uninstall.ps1" %*
pause
