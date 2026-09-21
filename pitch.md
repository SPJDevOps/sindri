# Leichtgewichtiges, cloud-natives Artifact Repository
### Gesprächsgrundlage für Sonntag

---

## 1. Die Idee in einem Satz

Ein modernes, **multi-protokoll-fähiges Artifact Repository**, das schlank bleibt, wo Nexus und Artifactory über die Jahre schwer geworden sind — startbar mit einer Docker-Compose-Datei, skalierbar bis zur Kubernetes-Enterprise-Installation. Ein Binary, keine zwei Produkte.

**Das Problem, das wir lösen:**
Nexus und Artifactory sind historisch gewachsen — schwergewichtig, komplex zu betreiben, überdimensioniert für kleine Teams. Kleinere Teams landen deshalb oft bei einem Flickwerk aus Harbor + Verdaccio + eigenem PyPI-Server, weil nichts dazwischen existiert. Für uns ist das die Lücke.

---

## 2. Warum jetzt, warum wir

- **Echter, spürbarer Frust** mit bestehenden Lösungen in der DevOps-Community
- **Harbor** deckt OCI gut ab, ist aber nicht wirklich multi-protokoll gedacht
- **JFrog/Nexus** zielen auf Enterprise — Preis und Komplexität schrecken kleine/mittlere Teams ab
- Unser Fokus: **klein starten, ehrlich wachsen** — vom Solo-Team bis zum Großkonzern, ohne Produktwechsel

---

## 3. Die drei Kernkonzepte

### 3.1 Identity-first statt File-first

Klassische Repos fragen: „Wo liegt die Datei?"
Wir fragen: „Was ist dieses Artefakt — Herkunft, Besitzer, Nachweise, Vertrauensstufe?"

```
payments-api:1.4.2
├── Identity   → sha256:abc123...
├── Owner      → Payments-Team
├── Herkunft   → Git commit abc123
├── Evidence   → SBOM, Signatur, Scan-Ergebnis
└── Lifecycle  → Dev ✓  Integration ✓  Prod ✗
```

**Der Clou:** Ein Artefakt wird nicht zwischen Repos *kopiert*, wenn es reift — sein Vertrauensstatus ändert sich.

```
                    Artefakt
                 sha256:abc123
                       │
          ┌────────────┼────────────┐
          ▼            ▼            ▼
        DEV       INTEGRATION      PROD
         ✓             ✓             ✗
```

Das beantwortet Kubernetes und CI/CD nicht mehr „Existiert dieses Image?", sondern
**„Ist genau dieses Artefakt für diese Umgebung freigegeben?"**

---

### 3.2 Ein Kern, viele Protokolle

Multi-Protokoll (OCI, npm, PyPI, Maven, Helm) ist Pflicht — aber nicht als lose Sammlung von Einzel-Features, sondern über einen gemeinsamen content-addressable Kern:

```
┌───────────────────────────────────────────┐
│           Protocol Adapters                │
│  OCI │ npm │ PyPI │ Maven │ Helm            │
└──────┬──────┬───────┬───────┬───────┬──────┘
       └──────┴───────┴───────┴───────┘
                     │
           ┌─────────▼─────────┐
           │  Core Artifact     │
           │  Store (CAS)       │
           │  sha256 → Blob     │
           └─────────┬─────────┘
                     │
           ┌─────────▼─────────┐
           │  Metadata / Index  │
           │  (Postgres)        │
           └───────────────────┘
```

**Vorteil:** Lifecycle, Promotion, Policies und Scanning existieren **einmal** im Kern — nicht fünfmal pro Protokoll. Genau die Komplexität, die wir Nexus vorwerfen, bauen wir uns so nicht selbst wieder ein.

---

### 3.3 Proxy / Hosted / Virtual — sauber getrennt

Die wichtigste Unterscheidung für Sicherheit: Was ist gespiegelt, was ist unseres?

```
┌─────────────┐   ┌─────────────┐   ┌──────────────────┐
│   PROXY      │   │   HOSTED     │   │  VIRTUAL/GROUP    │
│  (Spiegel)   │   │  (Eigenes)   │   │  (Entwickler-Sicht)│
├─────────────┤   ├─────────────┤   ├──────────────────┤
│ z.B. pypi-   │   │ z.B.         │   │ "python" =        │
│ proxy →      │   │ internal-    │   │ hosted + proxy,    │
│ pypi.org     │   │ python       │   │ feste Reihenfolge  │
└─────────────┘   └─────────────┘   └──────────────────┘
```

- **Promotion (Lifecycle) gilt nur für Hosted-Repos** — eigene Artefakte
- **Proxy-Repos werden nie promoted** — sie sind extern und bleiben es
- **Virtual-Repos** lösen mit fester, expliziter Reihenfolge auf (eigenes zuerst)

**Sicherheitsbonus quasi geschenkt:** Diese Trennung verhindert *Dependency Confusion* — den Angriff, bei dem ein interner Paketname von einem böswilligen öffentlichen Paket gleichen Namens überschattet wird. Ein Punkt, den viele Konkurrenzsysteme nur implizit lösen.

---

## 4. Konfiguration als Code (Team-Autonomie + Org-Governance)

Teams sollen ihre eigenen Repos bauen können — Organisationen sollen trotzdem Leitplanken setzen können. Beides gleichzeitig, über zwei Config-Ebenen:

```yaml
# Org-Ebene: verbindliche Leitplanken
kind: OrgPolicy
spec:
  mode: hybrid              # centralized | delegated | hybrid
  mandatoryRepos:
    - name: approved-base-images
      locked: true          # Teams referenzieren, überschreiben nicht

# Team-Ebene: eigene Virtual-Repos
kind: Repository
spec:
  type: virtual
  members:
    - ref: hosted/payments-internal
    - ref: org/approved-base-images
    - ref: proxy/pypi-public
  rbac:
    read:  [team:payments, team:platform]
    write: [team:payments-admins]
```

- **GitOps-Modell**: Config liegt in Git, ein Reconciler gleicht den Zustand ab → jede Rechteänderung geht durch Code-Review, nicht durch einen unsichtbaren Admin-Klick
- **Validierung beim Schreiben, nicht bei jedem Request** → schnell auf dem heißen Pfad (Downloads/Installs), Fehler werden früh beim PR abgefangen statt drei Wochen später beim Entwickler
- Nutzeridentität/Gruppenzugehörigkeit bleibt live an IdP (OIDC/LDAP) gebunden — Struktur ist statisch, wer reindarf ist dynamisch

---

## 5. Technischer Aufbau

**Sprache: Go** — Single-Binary, keine Runtime-Abhängigkeiten, starke Concurrency für parallele Uploads/Downloads und Hintergrundjobs, natürliche Nähe zu Kubernetes-Tooling.

**Ein Binary, mehrere Betriebsmodi** — kein zweites Produkt für „klein" vs. „groß":

```
myrepo serve               # alles in einem Prozess → Docker Compose
myrepo serve --api-only    # stateless, horizontal skalierbar → K8s
myrepo serve --worker-only # Scans, GC, Indexierung → K8s, per Queue skalierbar
```

**Kleininstallation (Compose):**
```
┌───────────────────────────┐
│        myrepo             │
│  API · Registry · Worker  │
└─────────────┬─────────────┘
              │
        ┌─────┴─────┐
        ▼           ▼
   PostgreSQL    Filesystem
```

**Enterprise-Installation (Kubernetes):**
```
                 Kubernetes
                     │
          ┌──────────┴──────────┐
          ▼                     ▼
     API-Pods (HPA)        Worker-Pods (KEDA)
          │                     │
          └──────────┬──────────┘
                     ▼
          PostgreSQL      S3-kompatibler Objektspeicher
```

Storage-Layer von Anfang an abstrahiert (Filesystem lokal, S3/GCS in der Cloud) — reine Config-Entscheidung, kein Codeunterschied.

---

## 6. Monetarisierung: Open Core

**Faustregel:** Alles, was ein einzelnes Team zum produktiven Arbeiten braucht, ist frei. Alles, was eine Organisation zur Governance über viele Teams hinweg braucht, ist bezahlt.

| Open Source (Apache 2.0 / MIT) | Enterprise / Paid |
|---|---|
| Alle Protokoll-Adapter (OCI, npm, PyPI, Maven, Helm) | SSO / SAML / LDAP |
| Storage-Backends (Filesystem, S3) | Audit-Logging mit Aufbewahrungspflicht |
| Basis-RBAC | Multi-Region / HA-Replikation |
| Voller Single-Node-Betrieb inkl. Compose | Erweiterte Policy-Engine (z.B. „kein Prod-Promote ohne SBOM") |
| | Managed Hosting / Cloud-Angebot |

**Warum diese Grenze funktioniert:**
Sie ist fair und nachvollziehbar statt willkürlich — Community-Vertrauen bleibt erhalten (siehe Gegenbeispiele: HashiCorp/Elastic-Lizenzwechsel führten zu Forks wie OpenTofu/OpenSearch). Eine echte OSI-Lizenz für den Kern signalisiert Ernsthaftigkeit und beschleunigt Adoption; die kommerzielle Schicht bleibt trotzdem sauber getrennt.

**Umsatztreiber in der Praxis meist:** nicht die Kernlizenz, sondern Managed Hosting und die Security/Policy-Schicht obendrauf.

---

## 7. Wettbewerbslandschaft — ehrliche Einordnung

- **Direkter Vergleichsmaßstab: Harbor** — nicht JFrog. Harbor ist OCI-stark, aber nicht multi-protokoll-nativ gedacht. Das ist unsere Lücke.
- **JFrog/Nexus/Sonatype**: Enterprise-Feature-Vollständigkeit, aber schwer, teuer, hohe Umstiegskosten — kein Direktangriff, sondern eine bewusst andere Zielgruppe zuerst
- **Strategie:** Open-Source-Adoption bei kleinen/mittleren Teams zuerst (wie Gitea/Forgejo gegenüber GitLab), Enterprise-Reife und Monetarisierung folgen danach

---

## 8. Die Kernbotschaft fürs Meeting

„Wir bauen nicht noch ein Nexus. Wir bauen das leichteste Multi-Protokoll-Repository, das mit einer Compose-Datei startet und bis zur Kubernetes-Enterprise-Installation mitwächst — mit Artefakt-Identität statt Datei-Ablage als Kernidee, und Governance als Code statt Klick-UI."


