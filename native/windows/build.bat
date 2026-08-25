@echo off
setlocal enabledelayedexpansion

rem Builds the capture helper. Invoked from the Linux side through WSL interop:
rem   cmd.exe /c native\windows\build.bat
rem
rem Probes for a toolchain that is actually complete rather than taking the
rem newest. On the target machine the 2022 and 18 installs have a vcvars64.bat
rem but no vcvarsall.bat for it to call, so "newest wins" picks an install that
rem cannot compile and reports a confusing error from one level down.

set "VCVARS="
for %%C in (
  "C:\Program Files\Microsoft Visual Studio\18\Enterprise\VC\Auxiliary\Build"
  "C:\Program Files\Microsoft Visual Studio\2022\Enterprise\VC\Auxiliary\Build"
  "C:\Program Files\Microsoft Visual Studio\2022\Professional\VC\Auxiliary\Build"
  "C:\Program Files\Microsoft Visual Studio\2022\Community\VC\Auxiliary\Build"
  "C:\Program Files (x86)\Microsoft Visual Studio\2019\Enterprise\VC\Auxiliary\Build"
  "C:\Program Files (x86)\Microsoft Visual Studio\2019\Professional\VC\Auxiliary\Build"
  "C:\Program Files (x86)\Microsoft Visual Studio\2019\Community\VC\Auxiliary\Build"
) do (
  if not defined VCVARS (
    if exist "%%~C\vcvarsall.bat" if exist "%%~C\vcvars64.bat" set "VCVARS=%%~C\vcvars64.bat"
  )
)

if not defined VCVARS (
  echo ERROR: no complete MSVC toolchain found.
  echo Looked for an install with both vcvarsall.bat and vcvars64.bat.
  exit /b 1
)

echo Using !VCVARS!
call "!VCVARS!" >nul
if errorlevel 1 (echo ERROR: vcvars failed & exit /b 1)

set "SRC=%~dp0capture.cpp"
set "OUTDIR=%~dp0..\..\dist"
if not exist "%OUTDIR%" mkdir "%OUTDIR%"

cl.exe /nologo /EHsc /O2 /W3 /MT /std:c++17 ^
  /D_CRT_SECURE_NO_WARNINGS ^
  "%SRC%" ^
  /Fe:"%OUTDIR%\minutes-capture.exe" ^
  /Fo:"%OUTDIR%\\" ^
  /link ole32.lib
if errorlevel 1 (echo ERROR: compile failed & exit /b 1)

del "%OUTDIR%\capture.obj" 2>nul
echo Built %OUTDIR%\minutes-capture.exe
