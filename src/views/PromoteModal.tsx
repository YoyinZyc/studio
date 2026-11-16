import { useEffect, useState } from 'react'
import {
  ArrowRight,
  Check,
  CircleCheck,
  CircleDashed,
  CircleX,
  GitBranch,
  Loader,
  Rocket,
  Server,
  TriangleAlert,
  Wrench,
  X,
} from 'lucide-react'
import { studioClient, type PromotePreflightReport } from '../api'
import { Toggle } from '../components'
import { shortName, type AXTask } from '../model/resources'
import { store, useStore } from '../model/store'

export function PromoteModal({ task, onClose }: { task: AXTask; onClose: () => void }) {
  const s = useStore()
  const candidates = s.contexts.filter((c) => c.name !== s.context)
  const [targetCtx, setTargetCtx] = useState(candidates[0]?.name ?? '')
  const [report, setReport] = useState<PromotePreflightReport | null>(null)
  const [checking, setChecking] = useState(false)
  const [pushingBranch, setPushingBranch] = useState(false)
  const [pushedBranch, setPushedBranch] = useState('')
  const [promoting, setPromoting] = useState(false)
  const [installingAX, setInstallingAX] = useState(false)
  const [handoffNote, setHandoffNote] = useState('')
  const [suspendSource, setSuspendSource] = useState(false)
  const [error, setError] = useState('')

  const runPreflight = async (ctxName: string) => {
    if (!ctxName) return
    setChecking(true)
    setError('')
    try {
      const rep = await studioClient.promotePreflight(s.context, ctxName, s.atespace, task.name)
      setReport(rep)
    } catch (e) {
      setReport(null)
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setChecking(false)
    }
  }

  useEffect(() => {
    if (targetCtx) void runPreflight(targetCtx)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [targetCtx])

  const handlePushBranchFirst = async () => {
    if (!report) return
    setPushingBranch(true)
    setError('')
    try {
      const res = await studioClient.pushTaskBranch(
        s.context,
        s.atespace,
        task.name,
        report.suggestedBranch || undefined
      )
      setPushedBranch(res.branch)
      await runPreflight(targetCtx)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setPushingBranch(false)
    }
  }

  const handleInstallAX = async () => {
    if (!targetCtx) return
    setInstallingAX(true)
    setError('')
    try {
      await studioClient.remoteInstall(targetCtx)
      // Poll briefly for completion, then re-run preflight.
      for (let i = 0; i < 45; i++) {
        await new Promise((r) => setTimeout(r, 2000))
        const st = await studioClient.getEnvStatus()
        if (st.operation?.status && st.operation.status !== 'running') break
      }
      await runPreflight(targetCtx)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setInstallingAX(false)
    }
  }

  const handlePromote = async () => {
    if (!targetCtx) return
    setPromoting(true)
    setError('')
    try {
      const res = await studioClient.promoteTask({
        sourceContext: s.context,
        targetContext: targetCtx,
        atespace: s.atespace,
        task: task.name,
        branchOverride: pushedBranch || undefined,
        handoffNote: handoffNote.trim() || undefined,
        suspendSource,
      })
      store.log(`Promoted ${task.name} → ${shortName(targetCtx)}`, `Created ${res.taskName} (session ${res.sessionId})`, true)
      onClose()
      store.setScope({ context: targetCtx, atespace: res.atespace })
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setPromoting(false)
    }
  }

  return (
    <div className="modal-backdrop" onMouseDown={onClose}>
      <div
        className="modal card fade-in"
        style={{ width: 'min(640px, 94vw)', maxHeight: '88vh', overflowY: 'auto' }}
        onMouseDown={(e) => e.stopPropagation()}
      >
        <div className="row" style={{ marginBottom: 12 }}>
          <Rocket size={17} color="var(--accent)" />
          <h3 style={{ margin: 0 }}>Run on a Team Cluster</h3>
          <span className="spacer" />
          <button className="btn ghost icon" onClick={onClose}>
            <X size={15} />
          </button>
        </div>
        <p className="muted" style={{ margin: '0 0 14px', lineHeight: 1.45 }}>
          Promote <strong>{task.name}</strong> from <code>{shortName(s.context || 'local')}</code> to a remote cluster
          with the exact same Workspace and Gateway rules, linked by shared <code>sessionId</code>.
        </p>

        {/* Step 1: Pick target cluster */}
        <div style={{ marginBottom: 14 }}>
          <label className="caption muted" style={{ display: 'block', marginBottom: 5, fontWeight: 600 }}>
            1. Target Kubernetes Cluster
          </label>
          {candidates.length === 0 ? (
            <div className="card" style={{ padding: 10, background: 'var(--bg-2)' }}>
              <span className="caption muted">
                No other cluster contexts found in <code>~/.kube/config</code>. Add a GKE or remote cluster context
                first.
              </span>
            </div>
          ) : (
            <div className="row" style={{ gap: 8 }}>
              <Server size={14} className="faint" />
              <select
                className="select"
                style={{ flex: 1 }}
                value={targetCtx}
                onChange={(e) => {
                  setPushedBranch('')
                  setTargetCtx(e.target.value)
                }}
              >
                {candidates.map((c) => (
                  <option key={c.name} value={c.name}>
                    {shortName(c.name)} ({c.name})
                  </option>
                ))}
              </select>
              <button className="btn small" disabled={checking || !targetCtx} onClick={() => void runPreflight(targetCtx)}>
                {checking ? 'Checking…' : 'Re-check'}
              </button>
            </div>
          )}
        </div>

        {/* Step 2: Live Preflight Checks */}
        <div style={{ marginBottom: 14 }}>
          <label className="caption muted" style={{ display: 'block', marginBottom: 6, fontWeight: 600 }}>
            2. Cluster & Sandbox Preflight Checks
          </label>
          {checking && (
            <div className="row muted" style={{ padding: 10, gap: 8 }}>
              <Loader size={14} className="spin" /> Running preflight checks against {shortName(targetCtx)}…
            </div>
          )}
          {report && !checking && (
            <div className="col" style={{ gap: 6 }}>
              {report.checks.map((c) => {
                const Icon =
                  c.status === 'ok' ? CircleCheck : c.status === 'warn' ? CircleDashed : CircleX
                const color =
                  c.status === 'ok' ? 'var(--green)' : c.status === 'warn' ? 'var(--warn)' : 'var(--red)'
                return (
                  <div
                    key={c.id}
                    className="card"
                    style={{ padding: '8px 10px', background: 'var(--bg-2)', display: 'flex', flexDirection: 'column', gap: 6 }}
                  >
                    <div className="row" style={{ gap: 8, alignItems: 'flex-start' }}>
                      <Icon size={15} color={color} style={{ marginTop: 2, flex: 'none' }} />
                      <div className="col" style={{ flex: 1, gap: 2 }}>
                        <strong style={{ fontSize: 12.5 }}>{c.label}</strong>
                        <span className="caption muted">{c.detail}</span>
                      </div>
                    </div>

                    {c.id === 'cluster_access' && report.canInstallAx && (
                      <div className="row" style={{ paddingLeft: 23 }}>
                        <button className="btn small primary" disabled={installingAX} onClick={() => void handleInstallAX()}>
                          <Wrench size={12} />
                          {installingAX ? 'Installing AX on cluster…' : `Install AX on ${shortName(targetCtx)}`}
                        </button>
                      </div>
                    )}

                    {c.id === 'uncommitted_changes' && (report.dirtyFiles?.length ?? 0) > 0 && !pushedBranch && (
                      <div className="row" style={{ paddingLeft: 23, gap: 8, flexWrap: 'wrap' }}>
                        <button
                          className="btn small primary"
                          disabled={pushingBranch}
                          onClick={() => void handlePushBranchFirst()}
                        >
                          <GitBranch size={12} />
                          {pushingBranch ? 'Pushing branch…' : `Push to ${report.suggestedBranch} first`}
                        </button>
                        <span className="caption faint">
                          Changed: {report.dirtyFiles!.slice(0, 3).join(', ')}
                          {report.dirtyFiles!.length > 3 ? ` +${report.dirtyFiles!.length - 3} more` : ''}
                        </span>
                      </div>
                    )}

                    {c.id === 'uncommitted_changes' && pushedBranch && (
                      <div className="row caption" style={{ paddingLeft: 23, color: 'var(--green)', gap: 5 }}>
                        <Check size={13} /> Pushed sandbox changes to <code>{pushedBranch}</code> — remote Workspace
                        will check out this branch!
                      </div>
                    )}
                  </div>
                )
              })}
            </div>
          )}
        </div>

        {/* Step 3: What carries over & Handoff Note */}
        {report && (
          <div style={{ marginBottom: 14 }}>
            <label className="caption muted" style={{ display: 'block', marginBottom: 6, fontWeight: 600 }}>
              3. What Carries Over & Handoff Note
            </label>
            <div className="card" style={{ padding: '8px 12px', background: 'var(--bg-2)', marginBottom: 10 }}>
              <div className="caption muted" style={{ display: 'grid', gridTemplateColumns: '120px 1fr', rowGap: 4 }}>
                <span>Workspace:</span>
                <code>
                  {report.workspaceName || 'empty /workspace'}
                  {pushedBranch ? ` (branch → ${pushedBranch})` : report.gitRef ? ` (ref: ${report.gitRef})` : ''}
                </code>
                <span>Guardrails:</span>
                <code>
                  {report.gatewayName
                    ? `${report.gatewayName} (${report.egressRuleCount} egress rule${report.egressRuleCount === 1 ? '' : 's'})`
                    : 'No external network (air-gapped)'}
                </code>
                <span>Session Thread:</span>
                <code>{report.sessionId || 'shared parent UID'} (starts fresh sandbox on remote cluster)</code>
              </div>
            </div>

            <textarea
              className="textarea"
              rows={2}
              placeholder="Optional handoff note appended to the remote task's goal (e.g. 'Run the full integration test suite with 4 CPUs')…"
              value={handoffNote}
              onChange={(e) => setHandoffNote(e.target.value)}
            />

            <div className="row" style={{ marginTop: 10, justifyContent: 'space-between' }}>
              <span className="caption muted">Suspend local task before starting remote run</span>
              <Toggle checked={suspendSource} onChange={setSuspendSource} />
            </div>
          </div>
        )}

        {error && (
          <div className="row problem-line" style={{ marginBottom: 12 }}>
            <TriangleAlert size={14} />
            <span className="selectable">{error}</span>
          </div>
        )}

        <div className="row" style={{ justifyContent: 'flex-end', gap: 8 }}>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button
            className="btn primary"
            disabled={!report?.canPromote || promoting || checking}
            onClick={() => void handlePromote()}
          >
            {promoting ? <Loader size={13} className="spin" /> : <ArrowRight size={13} />}
            {promoting ? 'Promoting…' : `Run on ${targetCtx ? shortName(targetCtx) : 'Cluster'}`}
          </button>
        </div>
      </div>
    </div>
  )
}
