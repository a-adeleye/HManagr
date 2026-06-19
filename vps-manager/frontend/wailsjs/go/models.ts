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

}

export namespace main {
	
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
	    ComposeFile: string;
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
	        this.ComposeFile = source["ComposeFile"];
	        this.Volumes = source["Volumes"];
	        this.EnvFiles = source["EnvFiles"];
	        this.ExternalNetworks = source["ExternalNetworks"];
	        this.BuildImages = source["BuildImages"];
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

