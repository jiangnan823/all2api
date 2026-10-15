@echo off
rem Double-click wrapper: runs install.ps1 next to this file.
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0install.ps1" %*
pause
