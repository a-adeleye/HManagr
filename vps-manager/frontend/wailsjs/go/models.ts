export namespace backup {
	
	export class BucketRef {
	    endpoint: string;
	    region: string;
	    bucket: string;
	    prefix: string;
	
	    static createFrom(source: any = {}) {
	        return new BucketRef(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.endpoint = source["endpoint"];
	        this.region = source["region"];
	        this.bucket = source["bucket"];
	        this.prefix = source["prefix"];
	    }
	}
	export class DBTarget {
	    container: string;
	    engine: string;
	    user: string;
	    db: string;
	
	    static createFrom(source: any = {}) {
	        return new DBTarget(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.container = source["container"];
	        this.engine = source["engine"];
	        this.user = source["user"];
	        this.db = source["db"];
	    }
	}
	export class RetentionPolicy {
	    keepDaily: number;
	    keepWeekly: number;
	    keepMonthly: number;
	    keepLast: number;
	
	    static createFrom(source: any = {}) {
	        return new RetentionPolicy(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.keepDaily = source["keepDaily"];
	        this.keepWeekly = source["keepWeekly"];
	        this.keepMonthly = source["keepMonthly"];
	        this.keepLast = source["keepLast"];
	    }
	}
	export class Job {
	    id: string;
	    name: string;
	    stackPath: string;
	    volumes: string[];
	    databases: DBTarget[];
	    includeCompose: boolean;
	    schedule: string;
	    retention: RetentionPolicy;
	    bucket: BucketRef;
	    enabled: boolean;
	    lastRun?: string;
	    lastStatus?: string;
	
	    static createFrom(source: any = {}) {
	        return new Job(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.stackPath = source["stackPath"];
	        this.volumes = source["volumes"];
	        this.databases = this.convertValues(source["databases"], DBTarget);
	        this.includeCompose = source["includeCompose"];
	        this.schedule = source["schedule"];
	        this.retention = this.convertValues(source["retention"], RetentionPolicy);
	        this.bucket = this.convertValues(source["bucket"], BucketRef);
	        this.enabled = source["enabled"];
	        this.lastRun = source["lastRun"];
	        this.lastStatus = source["lastStatus"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class RestoreOptions {
	    snapshotId: string;
	    targetPath: string;
	    restoreVolumes: boolean;
	    restoreCompose: boolean;
	    composeUp: boolean;
	    importDatabases: boolean;
	
	    static createFrom(source: any = {}) {
	        return new RestoreOptions(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.snapshotId = source["snapshotId"];
	        this.targetPath = source["targetPath"];
	        this.restoreVolumes = source["restoreVolumes"];
	        this.restoreCompose = source["restoreCompose"];
	        this.composeUp = source["composeUp"];
	        this.importDatabases = source["importDatabases"];
	    }
	}
	
	export class Secrets {
	    accessKey: string;
	    secretKey: string;
	    resticPassword: string;
	
	    static createFrom(source: any = {}) {
	        return new Secrets(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.accessKey = source["accessKey"];
	        this.secretKey = source["secretKey"];
	        this.resticPassword = source["resticPassword"];
	    }
	}
	export class Snapshot {
	    id: string;
	    time: string;
	    tags: string[];
	    paths: string[];
	
	    static createFrom(source: any = {}) {
	        return new Snapshot(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.time = source["time"];
	        this.tags = source["tags"];
	        this.paths = source["paths"];
	    }
	}

}

export namespace config {
	
	export class EnvVar {
	    key: string;
	    value: string;
	    secret: boolean;
	
	    static createFrom(source: any = {}) {
	        return new EnvVar(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.key = source["key"];
	        this.value = source["value"];
	        this.secret = source["secret"];
	    }
	}
	export class Deployment {
	    id: string;
	    name: string;
	    vpsId: string;
	    repoUrl: string;
	    branch: string;
	    path: string;
	    composeFile?: string;
	    githubToken?: string;
	    envVars: EnvVar[];
	    useSudo: boolean;
	    lastDeploy?: string;
	    lastStatus?: string;
	    lastCommit?: string;
	
	    static createFrom(source: any = {}) {
	        return new Deployment(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.vpsId = source["vpsId"];
	        this.repoUrl = source["repoUrl"];
	        this.branch = source["branch"];
	        this.path = source["path"];
	        this.composeFile = source["composeFile"];
	        this.githubToken = source["githubToken"];
	        this.envVars = this.convertValues(source["envVars"], EnvVar);
	        this.useSudo = source["useSudo"];
	        this.lastDeploy = source["lastDeploy"];
	        this.lastStatus = source["lastStatus"];
	        this.lastCommit = source["lastCommit"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	export class Project {
	    id: string;
	    name: string;
	    vpsId: string;
	    path: string;
	    database?: string;
	
	    static createFrom(source: any = {}) {
	        return new Project(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.vpsId = source["vpsId"];
	        this.path = source["path"];
	        this.database = source["database"];
	    }
	}
	export class VPS {
	    id: string;
	    name: string;
	    host: string;
	    port: number;
	    user: string;
	    authType: string;
	    keyPath?: string;
	    password?: string;
	    isLocal?: boolean;
	
	    static createFrom(source: any = {}) {
	        return new VPS(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.host = source["host"];
	        this.port = source["port"];
	        this.user = source["user"];
	        this.authType = source["authType"];
	        this.keyPath = source["keyPath"];
	        this.password = source["password"];
	        this.isLocal = source["isLocal"];
	    }
	}

}

export namespace db {
	
	export class Column {
	    name: string;
	    type: string;
	    nullable: boolean;
	    isPk: boolean;
	
	    static createFrom(source: any = {}) {
	        return new Column(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.type = source["type"];
	        this.nullable = source["nullable"];
	        this.isPk = source["isPk"];
	    }
	}
	export class Container {
	    id: string;
	    name: string;
	    image: string;
	    state: string;
	    engine: string;
	    user: string;
	    defaultDb: string;
	
	    static createFrom(source: any = {}) {
	        return new Container(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.image = source["image"];
	        this.state = source["state"];
	        this.engine = source["engine"];
	        this.user = source["user"];
	        this.defaultDb = source["defaultDb"];
	    }
	}
	export class QueryResult {
	    columns: string[];
	    rows: any[][];
	    message?: string;
	    total: number;
	
	    static createFrom(source: any = {}) {
	        return new QueryResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.columns = source["columns"];
	        this.rows = source["rows"];
	        this.message = source["message"];
	        this.total = source["total"];
	    }
	}
	export class Table {
	    schema: string;
	    name: string;
	
	    static createFrom(source: any = {}) {
	        return new Table(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.schema = source["schema"];
	        this.name = source["name"];
	    }
	}

}

export namespace docker {
	
	export class Container {
	    id: string;
	    names: string;
	    image: string;
	    status: string;
	    state: string;
	    ports: string;
	
	    static createFrom(source: any = {}) {
	        return new Container(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.names = source["names"];
	        this.image = source["image"];
	        this.status = source["status"];
	        this.state = source["state"];
	        this.ports = source["ports"];
	    }
	}
	export class TeardownOptions {
	    path: string;
	    project: string;
	    composeFiles: string[];
	    removeVolumes: boolean;
	    removeImages: boolean;
	    removeDir: boolean;
	
	    static createFrom(source: any = {}) {
	        return new TeardownOptions(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.project = source["project"];
	        this.composeFiles = source["composeFiles"];
	        this.removeVolumes = source["removeVolumes"];
	        this.removeImages = source["removeImages"];
	        this.removeDir = source["removeDir"];
	    }
	}

}

export namespace main {

	export class ConnectStatus {
	    connected: boolean;
	    needsTrust: boolean;
	    keyChanged: boolean;
	    fingerprint?: string;
	    keyType?: string;
	    host?: string;
	    message?: string;

	    static createFrom(source: any = {}) {
	        return new ConnectStatus(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.connected = source["connected"];
	        this.needsTrust = source["needsTrust"];
	        this.keyChanged = source["keyChanged"];
	        this.fingerprint = source["fingerprint"];
	        this.keyType = source["keyType"];
	        this.host = source["host"];
	        this.message = source["message"];
	    }
	}
	export class CommandResult {
	    stdout: string;
	    stderr: string;
	    exitCode: number;
	
	    static createFrom(source: any = {}) {
	        return new CommandResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.stdout = source["stdout"];
	        this.stderr = source["stderr"];
	        this.exitCode = source["exitCode"];
	    }
	}
	export class PathInfo {
	    mode: string;
	    uid: number;
	    gid: number;
	    owner: string;
	    group: string;
	    isDir: boolean;
	
	    static createFrom(source: any = {}) {
	        return new PathInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.mode = source["mode"];
	        this.uid = source["uid"];
	        this.gid = source["gid"];
	        this.owner = source["owner"];
	        this.group = source["group"];
	        this.isDir = source["isDir"];
	    }
	}

}

export namespace migration {
	
	export class ComposeContext {
	    project: string;
	    composeFiles: string[];
	
	    static createFrom(source: any = {}) {
	        return new ComposeContext(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.project = source["project"];
	        this.composeFiles = source["composeFiles"];
	    }
	}
	export class Volume {
	    name: string;
	    size: number;
	    mountpoint: string;
	
	    static createFrom(source: any = {}) {
	        return new Volume(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.size = source["size"];
	        this.mountpoint = source["mountpoint"];
	    }
	}
	export class Service {
	    name: string;
	    image: string;
	    isPostgres: boolean;
	    builds: boolean;
	
	    static createFrom(source: any = {}) {
	        return new Service(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.image = source["image"];
	        this.isPostgres = source["isPostgres"];
	        this.builds = source["builds"];
	    }
	}
	export class Inventory {
	    projectName: string;
	    services: Service[];
	    volumes: Volume[];
	    bindMounts: string[];
	    envFiles: string[];
	    externalNetworks: string[];
	    buildImages: string[];
	    warnings: string[];
	
	    static createFrom(source: any = {}) {
	        return new Inventory(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.projectName = source["projectName"];
	        this.services = this.convertValues(source["services"], Service);
	        this.volumes = this.convertValues(source["volumes"], Volume);
	        this.bindMounts = source["bindMounts"];
	        this.envFiles = source["envFiles"];
	        this.externalNetworks = source["externalNetworks"];
	        this.buildImages = source["buildImages"];
	        this.warnings = source["warnings"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class RunOptions {
	    SourcePath: string;
	    TargetPath: string;
	    ComposeFiles: string[];
	    ProjectName: string;
	    Volumes: string[];
	    EnvFiles: string[];
	    ExternalNetworks: string[];
	    BuildImages: string[];
	
	    static createFrom(source: any = {}) {
	        return new RunOptions(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.SourcePath = source["SourcePath"];
	        this.TargetPath = source["TargetPath"];
	        this.ComposeFiles = source["ComposeFiles"];
	        this.ProjectName = source["ProjectName"];
	        this.Volumes = source["Volumes"];
	        this.EnvFiles = source["EnvFiles"];
	        this.ExternalNetworks = source["ExternalNetworks"];
	        this.BuildImages = source["BuildImages"];
	    }
	}
	
	export class SubStack {
	    name: string;
	    path: string;
	    composeFile: string;
	
	    static createFrom(source: any = {}) {
	        return new SubStack(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.path = source["path"];
	        this.composeFile = source["composeFile"];
	    }
	}

}

export namespace sftp {
	
	export class FileInfo {
	    name: string;
	    path: string;
	    size: number;
	    mode: string;
	    isDir: boolean;
	    modTime: number;
	
	    static createFrom(source: any = {}) {
	        return new FileInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.path = source["path"];
	        this.size = source["size"];
	        this.mode = source["mode"];
	        this.isDir = source["isDir"];
	        this.modTime = source["modTime"];
	    }
	}

}

export namespace maintenance {

	export class DiskUsage {
	    filesystem: string;
	    totalBytes: number;
	    usedBytes: number;
	    availBytes: number;
	    usePercent: number;
	    mountPoint: string;
	    totalHuman: string;
	    usedHuman: string;
	    availHuman: string;
	    available: boolean;

	    static createFrom(source: any = {}) { return new DiskUsage(source); }
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.filesystem = source["filesystem"];
	        this.totalBytes = source["totalBytes"];
	        this.usedBytes = source["usedBytes"];
	        this.availBytes = source["availBytes"];
	        this.usePercent = source["usePercent"];
	        this.mountPoint = source["mountPoint"];
	        this.totalHuman = source["totalHuman"];
	        this.usedHuman = source["usedHuman"];
	        this.availHuman = source["availHuman"];
	        this.available = source["available"];
	    }
	}
	export class DockerCategory {
	    type: string;
	    totalCount: number;
	    activeCount: number;
	    sizeBytes: number;
	    reclaimableBytes: number;
	    sizeHuman: string;
	    reclaimableHuman: string;

	    static createFrom(source: any = {}) { return new DockerCategory(source); }
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.type = source["type"];
	        this.totalCount = source["totalCount"];
	        this.activeCount = source["activeCount"];
	        this.sizeBytes = source["sizeBytes"];
	        this.reclaimableBytes = source["reclaimableBytes"];
	        this.sizeHuman = source["sizeHuman"];
	        this.reclaimableHuman = source["reclaimableHuman"];
	    }
	}
	export class DockerUsage {
	    images: DockerCategory;
	    containers: DockerCategory;
	    volumes: DockerCategory;
	    buildCache: DockerCategory;
	    totalBytes: number;
	    reclaimableBytes: number;
	    totalHuman: string;
	    reclaimableHuman: string;
	    available: boolean;

	    static createFrom(source: any = {}) { return new DockerUsage(source); }
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.images = source["images"];
	        this.containers = source["containers"];
	        this.volumes = source["volumes"];
	        this.buildCache = source["buildCache"];
	        this.totalBytes = source["totalBytes"];
	        this.reclaimableBytes = source["reclaimableBytes"];
	        this.totalHuman = source["totalHuman"];
	        this.reclaimableHuman = source["reclaimableHuman"];
	        this.available = source["available"];
	    }
	}
	export class Usage {
	    disk: DiskUsage;
	    docker: DockerUsage;

	    static createFrom(source: any = {}) { return new Usage(source); }
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.disk = source["disk"];
	        this.docker = source["docker"];
	    }
	}
	export class PruneOptions {
	    allImages: boolean;
	    volumes: boolean;

	    static createFrom(source: any = {}) { return new PruneOptions(source); }
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.allImages = source["allImages"];
	        this.volumes = source["volumes"];
	    }
	}
	export class Image {
	    repository: string;
	    tag: string;
	    id: string;
	    sizeBytes: number;
	    sizeHuman: string;

	    static createFrom(source: any = {}) { return new Image(source); }
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.repository = source["repository"];
	        this.tag = source["tag"];
	        this.id = source["id"];
	        this.sizeBytes = source["sizeBytes"];
	        this.sizeHuman = source["sizeHuman"];
	    }
	}

}

export namespace provision {

	export class Engine {
	    kind: string;
	    label: string;
	    image: string;
	    tags: string[];
	    defaultPort: number;
	    mountPath: string;
	    needsUser: boolean;
	    needsDB: boolean;
	    needsAuth: boolean;
	    userEnvKey: string;
	    passEnvKey: string;
	    dbEnvKey: string;
	    defaultUser: string;
	    backupEngine: string;

	    static createFrom(source: any = {}) { return new Engine(source); }
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.kind = source["kind"];
	        this.label = source["label"];
	        this.image = source["image"];
	        this.tags = source["tags"];
	        this.defaultPort = source["defaultPort"];
	        this.mountPath = source["mountPath"];
	        this.needsUser = source["needsUser"];
	        this.needsDB = source["needsDB"];
	        this.needsAuth = source["needsAuth"];
	        this.userEnvKey = source["userEnvKey"];
	        this.passEnvKey = source["passEnvKey"];
	        this.dbEnvKey = source["dbEnvKey"];
	        this.defaultUser = source["defaultUser"];
	        this.backupEngine = source["backupEngine"];
	    }
	}
	export class Spec {
	    engine: string;
	    dir: string;
	    name: string;
	    tag: string;
	    user: string;
	    database: string;
	    password: string;
	    exposePort: number;
	    volumeName: string;

	    static createFrom(source: any = {}) { return new Spec(source); }
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.engine = source["engine"];
	        this.dir = source["dir"];
	        this.name = source["name"];
	        this.tag = source["tag"];
	        this.user = source["user"];
	        this.database = source["database"];
	        this.password = source["password"];
	        this.exposePort = source["exposePort"];
	        this.volumeName = source["volumeName"];
	    }
	}
	export class Result {
	    containerName: string;
	    service: string;
	    volumeName: string;
	    image: string;
	    password: string;
	    composeFile: string;
	    backupEngine: string;

	    static createFrom(source: any = {}) { return new Result(source); }
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.containerName = source["containerName"];
	        this.service = source["service"];
	        this.volumeName = source["volumeName"];
	        this.image = source["image"];
	        this.password = source["password"];
	        this.composeFile = source["composeFile"];
	        this.backupEngine = source["backupEngine"];
	    }
	}

}

export namespace caddy {

	export class ProxyInfo {
	    present: boolean;
	    container: string;
	    image: string;
	    network: string;

	    static createFrom(source: any = {}) { return new ProxyInfo(source); }
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.present = source["present"];
	        this.container = source["container"];
	        this.image = source["image"];
	        this.network = source["network"];
	    }
	}
	export class ExposeSpec {
	    stackPath: string;
	    mainCompose: string;
	    service: string;
	    domains: string[];
	    port: number;
	    network: string;
	    overrideName: string;

	    static createFrom(source: any = {}) { return new ExposeSpec(source); }
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.stackPath = source["stackPath"];
	        this.mainCompose = source["mainCompose"];
	        this.service = source["service"];
	        this.domains = source["domains"];
	        this.port = source["port"];
	        this.network = source["network"];
	        this.overrideName = source["overrideName"];
	    }
	}

}

