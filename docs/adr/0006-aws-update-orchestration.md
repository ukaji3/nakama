# ADR 0006: `nakama aws update` のオーケストレーション方針

- Status: Accepted
- Date: 2026-05-31
- Context: nakama v1.x 機能追加 (`update`)

## Context

既存インスタンスの一括最新化 (code-server の latest 更新 +
`apt-get dist-upgrade`) を 1 コマンドで行いたい。インスタンスは
private subnet にあり、更新は SSM Run Command (`AWS-RunShellScript`)
で実行する。`nakama aws edit --ebs` の growpart 実行で同経路の実績がある。
更新は破壊的になり得るため、安全策と電源状態のハンドリングが要る。

## Decision

1. **更新前に自動バックアップ**: `CreateBackup` を更新の直前に必ず実行する
   (label `preupdate-<UTC yyyy-mm-dd-HHMM>`, note 付与, `--keep` 連動)。
   スナップショットの `completed` は待たない — `CreateSnapshot` は API
   呼出時点の point-in-time を捉えるため、復元点としては `pending` でも有効。
2. **電源状態の自動管理**: 対象は running / stopped のみ受理。stopped は
   start→更新→stop と遷移し、**元の電源状態へ戻す**。更新スクリプトが
   失敗しても電源状態は元へ復帰させる (バックアップ済のため復旧可能)。
3. **code-server は latest 固定**: バージョン指定フラグは設けない
   (特定版固定/ダウングレードは非対応)。
4. **apt は `dist-upgrade -y` を非対話で**: `DEBIAN_FRONTEND=noninteractive`
   と `--force-confdef`/`--force-confold` でプロンプト停止を回避。
5. **reboot は needs-restart 連動 + 電源状態依存**:
   `/var/run/reboot-required` の有無で判定し、
   - 元 running かつ要再起動 → `RebootInstances` を実行 (running 維持のため反映に必要)
   - 元 stopped → reboot しない。最終 stop と利用者の次回 start が完全な
     電源サイクルとなり、新カーネルはそこでロードされるため別途 reboot は冗長。
6. **start 直後の SSM エージェント未登録**に備え、`SendCommand` を
   `InvalidInstanceId` の間リトライする (online 待ち)。`edit --ebs` は
   稼働中前提だったため、この待ちは `update` で新設。
7. **長時間化対応**: `AWS-RunShellScript` の `executionTimeout` と
   ポーリング Timeout を `dist-upgrade` 想定で延長する。

## Consequences

- 追加 AWS 操作は `ec2:RebootInstances` のみ (instance role/呼び出し側 IAM は
  既存で充足。SSM Run Command は `edit` で実績あり)。
- 更新は破壊的だが、直前バックアップ + `restore` で巻き戻し可能。
- `update` の度にスナップショットが増えるため `--keep N` で世代制御できる。
- 元 stopped で要再起動の場合、新カーネルは「次回 start 時」に反映される
  (update コマンド内では反映されない)。結果出力で明示する。

## Alternatives considered

- **手動 (`connect` 内で利用者が実行)**: 追加実装ゼロだが、バックアップ・
  電源管理・version 書き戻しが手作業になり非効率。`update` の自動化価値が出ない。
- **常に reboot / 一切 reboot しない**: 前者は毎回ダウンタイム、後者は
  カーネル更新が未反映のまま running し得る。needs-restart 連動 (option c) を採用。
- **スナップショット完了待ち**: 数分のブロッキングが増えるだけで復元点の
  有効性は変わらないため不採用。

## 追記 (2026-10-20, Issue #101)

update は AWS CLI (公式のインストールがあるとき) と Kiro Crew (`kirocrew update` / `kirocrew restart`) も更新するようになった。また、詳しい出力はインスタンスの `/var/log/nakama-update.log` に書き、Run Command の stdout には結果の印だけを出すようにした (GetCommandInvocation が stdout の先頭 24,000 字しか返さず、再起動の要否の印が切れることがあったため)。ADR 0024 を参照。
