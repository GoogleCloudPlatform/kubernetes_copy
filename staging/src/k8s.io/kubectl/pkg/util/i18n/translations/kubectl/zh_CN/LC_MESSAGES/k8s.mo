Þ    v      Ì     |      ð	  z   ñ	  <   l
  S   ©
  <   ý
  c  :  ´    .   S  V     "   Ù  4   ü     1     N    m  X   s  o   Ì    <  v   >  t   µ  Ä  *  ;   ï  [   +  J     a   Ò  ½   4  Å   ò  ®   ¸  9   g  %   ¡  W   Ç       M   =  u     4     -   6  3   d  1     2   Ê     ý  *     +   <  .   h  0     +   È  A   ô  ^   6  0     0   Æ  "   ÷       8   8  *   q  A        Þ  #   ü  )         J     h        !   £  D   Å  (   
     3  `   J     «     G      ß       ë      !     %!  $   @!     e!     !  a   !  s   ý!  U   q"  +   Ç"  +   ó"  6   #  q   V#  /   È#  1   ø#  '   *$  /   R$  0   $  N   ³$  O   %  /   R%  (   %     «%  &   Ä%  %   ë%  (   &  #   :&  G   ^&      ¦&     Ç&  9   æ&      '  c   ?'  #   £'  H   Ç'  &   (  e   7(  E   (  a   ã(     E)     b)     z)  =   )  $   Ô)     ù)  &   *  +   @*     l*  r   *     ô*  /   +    8+  y   »,  8   5-  A   n-  6   °-  a  ç-     I/  -   ê0  h   1  -   1  0   ¯1     à1  !   ÿ1  í   !2  a   3  r   q3  ×   ä3     ¼4  {   C5  Û  ¿5  C   7  Z   ß7  F   :8  Z   8  ¨   Ü8  Á   9  ¢   G:  =   ê:     (;  \   G;     ¤;  f   Ã;  m   *<  +   <  $   Ä<  /   é<  ?   =  B   Y=     =  *   ´=  =   ß=  0   >  '   N>  )   v>  <    >  M   Ý>  '   +?  *   S?     ~?     ?  3   ¼?  '   ð?  E   @     ^@  #   z@     @     º@  &   Ö@     ý@  '   A  B   DA  +   A     ³A  ^   ÏA     .B     ·B     <C     IC     _C     xC  '   C     ¼C     ÒC  o   ëC  v   [D  O   ÒD  ,   "E  %   OE  *   uE  `    E  *   F  !   ,F     NF  &   lF  )   F  ?   ½F  B   ýF  $   @G  -   eG     G      ©G     ÊG  4   éG  "   H  Q   AH     H     ¯H  -   ÈH     öH  c   I     pI  ;   I     ÌI  Q   ëI  :   =J  N   xJ     ÇJ     âJ     ûJ  3   K     EK     aK  *   zK  /   ¥K     ÕK  u   èK     ^L  )   rL        k   b         ]       -   1   Q   K   O   &   G       F   .   (   V              8   m   !   R   c       *      $       ,   C   +      o   %       B          r       ?   A   i   <   E      g                    >       s   I          3   P   U   N           '   d   `                  W      e   @   Y   
       7   q   Z       [      H                  9   2      v          "   _                            =   L      0   4   ;      f      M          a   p   n       T   S   t               5   j   D   6      l   h             #       \   /       	   ^       J       X   :   u           )    
		  # Show metrics for all nodes
		  kubectl top node

		  # Show metrics for a given node
		  kubectl top node NODE_NAME 
		# Print flags inherited by all commands
		kubectl options 
		# Print the client and server versions for the current context
		kubectl version 
		# Print the supported API versions
		kubectl api-versions 
		# Show metrics for all pods in the default namespace
		kubectl top pod

		# Show metrics for all pods in the given namespace
		kubectl top pod --namespace=NAMESPACE

		# Show metrics for a given pod and its containers
		kubectl top pod POD_NAME --containers

		# Show metrics for the pods defined by label name=myLabel
		kubectl top pod -l name=myLabel 
		Convert config files between different API versions. Both YAML
		and JSON formats are accepted.

		The command takes filename, directory, or URL as input, and convert it into format
		of version specified by --output-version flag. If target version is not specified or
		not supported, convert to latest version.

		The default output will be printed to stdout in YAML format. One can use -o option
		to change to output destination. 
		Create a namespace with the specified name. 
		Create a resource from a file or from stdin.

		JSON and YAML formats are accepted. 
		Create a role with single rule. 
		Create a service account with the specified name. 
		Mark node as schedulable. 
		Mark node as unschedulable. 
		Set the latest last-applied-configuration annotations by setting it to match the contents of a file.
		This results in the last-applied-configuration being updated as though 'kubectl apply -f <file>' was run,
		without updating any other parts of the object. 
	  # Create a new namespace named my-namespace
	  kubectl create namespace my-namespace 
	  # Create a new service account named my-service-account
	  kubectl create serviceaccount my-service-account 
	Create an ExternalName service with the specified name.

	ExternalName service references to an external DNS address instead of
	only pods, which will allow application authors to reference services
	that exist off platform, on other clusters, or locally. 
	Help provides help for any command in the application.
	Simply type kubectl help [path to command] for full details. 
    # Create a new LoadBalancer service named my-lbs
    kubectl create service loadbalancer my-lbs --tcp=5678:8080 
    # Dump current cluster state to stdout
    kubectl cluster-info dump

    # Dump current cluster state to /path/to/cluster-state
    kubectl cluster-info dump --output-directory=/path/to/cluster-state

    # Dump all namespaces to stdout
    kubectl cluster-info dump --all-namespaces

    # Dump a set of namespaces to /path/to/cluster-state
    kubectl cluster-info dump --namespaces default,kube-system --output-directory=/path/to/cluster-state 
    Create a LoadBalancer service with the specified name. A comma-delimited set of quota scopes that must all match each object tracked by the quota. A comma-delimited set of resource=quantity pairs that define a hard limit. A label selector to use for this budget. Only equality-based selector requirements are supported. A label selector to use for this service. Only equality-based selector requirements are supported. If empty (the default) infer the selector from the replication controller or replica set.) Additional external IP address (not managed by Kubernetes) to accept for the service. If this IP is routed to a node, the service can be accessed by this IP in addition to its generated service IP. An inline JSON override for the generated object. If this is non-empty, it is used to override the generated object. Requires that the object supply a valid apiVersion field. Apply a configuration to a resource by file name or stdin Approve a certificate signing request Assign your own ClusterIP or set to 'None' for a 'headless' service (no loadbalancing). Attach to a running container Auto-scale a deployment, replica set, stateful set, or replication controller ClusterIP to be assigned to the service. Leave empty to auto-allocate, or set to 'None' to create a headless service. ClusterRole this ClusterRoleBinding should reference ClusterRole this RoleBinding should reference Convert config files between different API versions Copy files and directories to and from containers Copy files and directories to and from containers. Create a TLS secret Create a namespace with the specified name Create a resource from a file or from stdin Create a secret for use with a Docker registry Create a service account with the specified name Create and run a particular image in a pod. Create debugging sessions for troubleshooting workloads and nodes Delete resources by file names, stdin, resources and names, or by resources and label selector Delete the specified cluster from the kubeconfig Delete the specified context from the kubeconfig Deny a certificate signing request Describe one or many contexts Diff the live version against a would-be applied version Display clusters defined in the kubeconfig Display merged kubeconfig settings or a specified kubeconfig file Display one or many resources Display resource (CPU/memory) usage Drain node in preparation for maintenance Edit a resource on the server Email for Docker registry Execute a command in a container Execute a command in a container. Experimental: Wait for a specific condition on one or many resources Forward one or more local ports to a pod Help about any command If non-empty, set the session affinity for the service to this; legal values: 'None', 'ClientIP' If non-empty, the annotation update will only succeed if this is the current resource-version for the object. Only valid when specifying a single resource. If non-empty, the labels update will only succeed if this is the current resource-version for the object. Only valid when specifying a single resource. List events Manage the rollout of a resource Mark node as schedulable Mark node as unschedulable Mark the provided resource as paused Modify certificate resources. Modify kubeconfig files Name or number for the port on the container that the service should direct traffic to. Optional. Only return logs after a specific date (RFC3339). Defaults to all logs. Only one of since-time / since may be used. Output shell completion code for the specified shell (bash, zsh, fish, or powershell) Password for Docker registry authentication Path to PEM encoded public key certificate. Path to private key associated with given certificate. Precondition for resource version. Requires that the current resource version match this value in order to scale. Print the client and server version information Print the list of flags inherited by all commands Print the logs for a container in a pod Print the supported API resources on the server Print the supported API resources on the server. Print the supported API versions on the server, in the form of "group/version" Print the supported API versions on the server, in the form of "group/version". Provides utilities for interacting with plugins Replace a resource by file name or stdin Resume a paused resource Role this RoleBinding should reference Run a particular image on the cluster Run a proxy to the Kubernetes API server Server location for Docker registry Set a new size for a deployment, replica set, or replication controller Set specific features on objects Set the selector on a resource Show details of a specific resource or group of resources Show the status of the rollout Take a replication controller, service, deployment or pod and expose it as a new Kubernetes service The image for the container to run. The minimum number or percentage of available pods this budget requires. The name for the newly created object. The name for the newly created object. If not specified, the name of the input resource will be used. The network protocol for the service to be created. Default is 'TCP'. The port that the service should serve on. Copied from the resource being exposed, if unspecified The type of secret to create Undo a previous rollout Update fields of a resource Update resource requests/limits on objects with pod templates Update the annotations on a resource Update the labels on a resource Update the taints on one or more nodes Username for Docker registry authentication View rollout history Where to output the files.  If empty or '-' uses stdout, otherwise creates a directory hierarchy in that directory dummy restart flag) kubectl controls the Kubernetes cluster manager Project-Id-Version: gettext-go-examples-hello
Report-Msgid-Bugs-To: EMAIL
PO-Revision-Date: 2023-09-03 23:46+0800
Last-Translator: zhengjiajin <zhengjiajin@caicloud.io>
Language-Team: 
Language: zh
MIME-Version: 1.0
Content-Type: text/plain; charset=UTF-8
Content-Transfer-Encoding: 8bit
Plural-Forms: nplurals=2; plural=(n > 1);
X-Generator: Poedit 3.3.2
X-Poedit-SourceCharset: UTF-8
 
		  # æ¾ç¤ºææèç¹çææ 
		  kubectl top node

		  # æ¾ç¤ºæå®èç¹çææ 
		  kubectl top node NODE_NAME 
		# è¾åºææå½ä»¤ç»§æ¿ç flags
		kubectl options 
		# è¾åºå½åå®¢æ·ç«¯åæå¡ç«¯ççæ¬
		kubectl version 
		# è¾åºæ¯æç API çæ¬
		kubectl api-versions 
		# æ¾ç¤º default å½åç©ºé´ä¸ææ Pods çææ 
		kubectl top pod

		# æ¾ç¤ºæå®å½åç©ºé´ä¸ææ Pods çææ 
		kubectl top pod --namespace=NAMESPACE

		# æ¾ç¤ºæå® Pod åå®çå®¹å¨ç metrics
		kubectl top pod POD_NAME --containers

		# æ¾ç¤ºæå® label ä¸º name=myLabel ç Pods ç metrics
		kubectl top pod -l name=myLabel 
		å¨ä¸åç API çæ¬ä¹é´è½¬æ¢éç½®æä»¶ãæ¥å YAML
		å JSON æ ¼å¼ã

		è¿ä¸ªå½ä»¤ä»¥æä»¶å, ç®å½, æè URL ä½ä¸ºè¾å¥ï¼å¹¶éè¿ âoutput-version åæ°
		 è½¬æ¢å°æå®çæ¬çæ ¼å¼ãå¦ææ²¡ææå®ç®æ çæ¬æèææå®çæ¬
		ä¸æ¯æ, åè½¬æ¢ä¸ºææ°çæ¬ã

		é»è®¤ä»¥ YAML æ ¼å¼è¾åºå°æ åè¾åºãå¯ä»¥ä½¿ç¨ -o option
		ä¿®æ¹ç®æ è¾åºçæ ¼å¼ã 
		ç¨ç»å®åç§°åå»ºä¸ä¸ªå½åç©ºé´ã 
		éè¿æä»¶åæèæ åè¾å¥æµ(stdin)åå»ºä¸ä¸ªèµæº.

		JSON and YAML formats are accepted. 
		åå»ºä¸ä¸ªå·æåä¸è§åçè§è²ã 
		ç¨æå®çåç§°åå»ºä¸ä¸ªæå¡è´¦æ·ã 
		æ è®°èç¹ä¸ºå¯è°åº¦ã 
		æ è®°èç¹ä¸ºä¸å¯è°åº¦ã 
		è®¾ç½®ææ°ç last-applied-configuration æ³¨è§£ï¼ä½¿ä¹å¹éææä»¶çåå®¹ã
		è¿ä¼å¯¼è´ last-applied-configuration è¢«æ´æ°ï¼å°±åæ§è¡äº kubectl apply -f <file> ä¸æ ·ï¼
		åªæ¯ä¸ä¼æ´æ°å¯¹è±¡çå¶ä»é¨åã 
	  # åå»ºä¸ä¸ªåä¸º my-namespace çæ°å½åç©ºé´
	  kubectl create namespace my-namespace 
	  # åå»ºä¸ä¸ªåä¸º my-service-account çæ°æå¡å¸æ·
	  kubectl create serviceaccount my-service-account 
	åå»ºå·ææå®åç§°ç ExternalName æå¡ã

	ExternalName æå¡å¼ç¨å¤é¨ DNS å°åèä¸æ¯ Pod å°åï¼
	è¿å°åè®¸åºç¨ç¨åºä½èå¼ç¨å­å¨äºå¹³å°å¤ãå¶ä»éç¾¤ä¸ææ¬å°çæå¡ã 
	Help ä¸ºåºç¨ç¨åºä¸­çä»»ä½å½ä»¤æä¾å¸®å©ã
	åªéé®å¥ kubectl help [å½ä»¤è·¯å¾] å³å¯è·å¾å®æ´çè¯¦ç»ä¿¡æ¯ã 
    # åå»ºä¸ä¸ªåç§°ä¸º my-lbs çæ°è´è½½åè¡¡æå¡
    kubectl create service loadbalancer my-lbs --tcp=5678:8080 
    # å¯¼åºå½åçéç¾¤ç¶æä¿¡æ¯å°æ åè¾åº
    kubectl cluster-info dump

    # å¯¼åºå½åçéç¾¤ç¶æå° /path/to/cluster-state
    kubectl cluster-info dump --output-directory=/path/to/cluster-state

    # å¯¼åºææå½åç©ºé´å°æ åè¾åº
    kubectl cluster-info dump --all-namespaces

    # å¯¼åºä¸ç»å½åç©ºé´å° /path/to/cluster-state
    kubectl cluster-info dump --namespaces default,kube-system --output-directory=/path/to/cluster-state 
    ä½¿ç¨ä¸ä¸ªæå®çåç§°åå»ºä¸ä¸ª LoadBalancer æå¡ã ä¸ç»ä»¥éå·åéçéé¢èå´ï¼å¿é¡»å¨é¨å¹ééé¢æè·è¸ªçæ¯ä¸ªå¯¹è±¡ã ä¸ç»ä»¥éå·åéçèµæº=æ°éå¯¹ï¼ç¨äºå®ä¹ç¡¬æ§éå¶ã ä¸ä¸ªç¨äºè¯¥é¢ç®çæ ç­¾éæ©å¨ãåªæ¯æåºäºç­å¼æ¯è¾çéæ©å¨è¦æ±ã ç¨äºæ­¤æå¡çæ ç­¾éæ©å¨ãä»æ¯æåºäºç­å¼æ¯è¾çéæ©å¨è¦æ±ãå¦æä¸ºç©ºï¼é»è®¤ï¼ï¼åä»å¯æ¬æ§å¶å¨æå¯æ¬éä¸­æ¨æ­éæ©å¨ãï¼ ä¸ºæå¡ææ¥åçå¶ä»å¤é¨ IP å°åï¼ä¸ç± Kubernetes ç®¡çï¼ãå¦æè¿ä¸ª IP è¢«è·¯ç±å°ä¸ä¸ªèç¹ï¼é¤äºå¶çæçæå¡ IP å¤ï¼è¿å¯ä»¥éè¿è¿ä¸ª IP è®¿é®æå¡ã éå¯¹æçæå¯¹è±¡çåè JSON è¦çãå¦æè¿ä¸å¯¹è±¡æ¯éç©ºçï¼å°ç¨äºè¦çæçæçå¯¹è±¡ãè¦æ±å¯¹è±¡æä¾ææç apiVersion å­æ®µã éè¿æä»¶åææ åè¾å¥æµ(stdin)å¯¹èµæºè¿è¡éç½® æ¹åä¸ä¸ªè¯ä¹¦ç­¾ç½²è¯·æ± ä¸ºâæ å¤´âæå¡ï¼æ è´è½½å¹³è¡¡ï¼åéä½ èªå·±ç ClusterIP æè®¾ç½®ä¸ºâæ ã è¿æ¥å°æ­£å¨è¿è¡çå®¹å¨ èªå¨è°æ´ä¸ä¸ª deployment, replica set, stateful set æè replication controller çå¯æ¬æ°é è¦åéç»æå¡ç ClusterIPãçç©ºè¡¨ç¤ºèªå¨åéï¼æè®¾ç½®ä¸º âNoneâ ä»¥åå»ºæ å¤´æå¡ã ClusterRoleBinding åºè¯¥æå® ClusterRole RoleBinding åºè¯¥æå® ClusterRole å¨ä¸åç API çæ¬ä¹é´è½¬æ¢éç½®æä»¶ å°æä»¶åç®å½å¤å¶å°å®¹å¨ä¸­æä»å®¹å¨ä¸­å¤å¶åºæ¥ å°æä»¶åç®å½å¤å¶å°å®¹å¨ä¸­æä»å®¹å¨ä¸­å¤å¶åºæ¥ã åå»ºä¸ä¸ª TLS secret ç¨æå®çåç§°åå»ºä¸ä¸ªå½åç©ºé´ éè¿æä»¶åæèæ åè¾å¥æµ(stdin)åå»ºä¸ä¸ªèµæº åå»ºä¸ä¸ªç» Docker registry ä½¿ç¨ç Secret åå»ºä¸ä¸ªæå®åç§°çæå¡è´¦æ· å¨ pod ä¸­åå»ºå¹¶è¿è¡ç¹å®éåã åå»ºè°è¯ä¼è¯ï¼ä»¥æé¤å·¥ä½è´è½½åèç¹çæé ææä»¶åãstdinãèµæºååç§°æèµæºåæ ç­¾éæ©å¨å é¤èµæº ä» kubeconfig ä¸­å é¤æå®çéç¾¤ ä» kubeconfig ä¸­å é¤æå®çä¸ä¸æ æç»ä¸ä¸ªè¯ä¹¦ç­¾åè¯·æ± æè¿°ä¸ä¸ªæå¤ä¸ªä¸ä¸æ å°å®æ¶çæ¬ä¸å¯è½åºç¨ççæ¬è¿è¡æ¯è¾ æ¾ç¤ºå¨ kubeconfig ä¸­å®ä¹çéç¾¤ æ¾ç¤ºåå¹¶ç kubeconfig éç½®æä¸ä¸ªæå®ç kubeconfig æä»¶ æ¾ç¤ºä¸ä¸ªæå¤ä¸ªèµæº æ¾ç¤ºèµæº (CPU/Memory) ä½¿ç¨é æ¸ç©ºèç¹ä»¥åå¤ç»´æ¤ ç¼è¾æå¡ç«¯ä¸çèµæº ç¨äº Docker éååºçé®ä»¶å°å å¨å®¹å¨ä¸­æ§è¡ä¸ä¸ªå½ä»¤ å¨æä¸ªå®¹å¨ä¸­æ§è¡ä¸ä¸ªå½ä»¤ã å®éªæ§ï¼å¨ä¸ä¸ªæå¤ä¸ªèµæºä¸ç­å¾ç¹å®æ¡ä»¶çåºç° å°ä¸ä¸ªæå¤ä¸ªæ¬å°ç«¯å£è½¬åå° pod å³äºä»»ä½å½ä»¤çå¸®å© å¦æéç©ºï¼åå°æå¡çä¼è¯äº²åæ§è®¾ç½®ä¸ºæ­¤å¼ï¼åæ³å¼ï¼'None'ã'ClientIP' å¦æéç©ºï¼ååªæå½æç»å¼æ¯å¯¹è±¡çå½åèµæºçæ¬æ¶ï¼æ³¨è§£æ´æ°æä¼æåã ä»å¨æå®åä¸ªèµæºæ¶ææã å¦æéç©ºï¼åæ ç­¾æ´æ°åªæå¨æç»å¼æ¯å¯¹è±¡çå½åèµæºçæ¬æ¶æä¼æåãä»å¨æå®åä¸ªèµæºæ¶ææã ååºäºä»¶ ç®¡çèµæºçä¸çº¿ æ è®°èç¹ä¸ºå¯è°åº¦ æ è®°èç¹ä¸ºä¸å¯è°åº¦ å°ææå®çèµæºæ è®°ä¸ºå·²æå ä¿®æ¹è¯ä¹¦èµæºã ä¿®æ¹ kubeconfig æä»¶ æ­¤ä¸ºç«¯å£çåç§°æç«¯å£å·ï¼æå¡åºå°æµéå®åå°å®¹å¨ä¸çè¿ä¸ç«¯å£ãæ­¤å±æ§ä¸ºå¯éã ä»è¿åå¨æå®æ¥æ (RFC3339) ä¹åçæ¥å¿ãé»è®¤ä¸ºæææ¥å¿ãåªè½ä½¿ç¨ since-time / since ä¹ä¸ã ä¸ºæå®ç shell(bash, zsh, fish, or powershell) è¾åº shell è¡¥å¨ä»£ç ã ç¨äº Docker éååºèº«ä»½éªè¯çå¯ç  PEM ç¼ç çå¬é¥è¯ä¹¦çè·¯å¾ã ä¸ç»å®è¯ä¹¦å³èçç§é¥çè·¯å¾ã èµæºçæ¬çåææ¡ä»¶ãè¦æ±å½åèµæºçæ¬ä¸æ­¤å¼å¹éæè½è¿è¡æ©ç¼©æä½ã è¾åºå®¢æ·ç«¯åæå¡ç«¯ççæ¬ä¿¡æ¯ è¾åºææå½ä»¤çå±çº§å³ç³» æå° Pod ä¸­å®¹å¨çæ¥å¿ æå°æå¡ç«¯ä¸æ¯æç API èµæº æå°æå¡ç«¯ä¸æ¯æç API èµæºã ä»¥âç»/çæ¬âçå½¢å¼æå°æå¡ç«¯æ¯æç API çæ¬ ä»¥âç»/çæ¬âçå½¢å¼æå°æå¡ç«¯æ¯æç API çæ¬ã æä¾ä¸æä»¶äº¤äºçå®ç¨ç¨åº éè¿æä»¶åæè stdinæ¿æ¢ä¸ä¸ªèµæº æ¢å¤æåçèµæº RoleBinding åºè¯¥å¼ç¨ç Role å¨éç¾¤ä¸è¿è¡ç¹å®éå è¿è¡ä¸ä¸ªæå Kubernetes API æå¡å¨çä»£ç Docker éååºçæå¡å¨ä½ç½® ä¸º deployment, replicaSet, replication controller è®¾ç½®ä¸ä¸ªæ°çå¯æ¬æ°é ä¸ºå¯¹è±¡è®¾ç½®æå®ç¹æ§ ä¸ºèµæºè®¾ç½®éæ©å¨ æ¾ç¤ºç¹å®èµæºæèµæºç»çè¯¦ç»ä¿¡æ¯ æ¾ç¤ºä¸çº¿çç¶æ å° replication controller, service, deployment æè pod æ´é²ä¸ºä¸ä¸ªæ°ç Kubernetes service æå®å®¹å¨è¦è¿è¡çéå. æ­¤é¢ç®è¦æ±çå¯ç¨ Pod çæå°æ°éæç¾åæ¯ã æ°åå»ºçå¯¹è±¡çåç§°ã æ°åå»ºçå¯¹è±¡çåç§°ãå¦ææªæå®ï¼å°ä½¿ç¨è¾å¥èµæºçåç§°ã è¦åå»ºçæå¡çç½ç»åè®®ãé»è®¤ä¸º âTCPâã æå¡è¦ä½¿ç¨çç«¯å£ãå¦ææ²¡ææå®ï¼åä»è¢«æ´é²çèµæºå¤å¶ è¦åå»ºç Secret ç±»å æ¤éä¸ä¸æ¬¡çä¸çº¿ æ´æ°èµæºçå­æ®µ ä½¿ç¨ Pod æ¨¡æ¿æ´æ°å¯¹è±¡çèµæºè¯·æ±/éå¶ æ´æ°ä¸ä¸ªèµæºçæ³¨è§£ æ´æ°èµæºä¸çæ ç­¾ æ´æ°ä¸ä¸ªæèå¤ä¸ªèç¹ä¸çæ±¡ç¹ ç¨äº Docker éååºèº«ä»½éªè¯çç¨æ·å æ¾ç¤ºä¸çº¿åå² å¨åªéè¾åºæä»¶ãå¦æä¸ºç©ºæ â-â åä½¿ç¨æ åè¾åºï¼å¦åå¨è¯¥ç®å½ä¸­åå»ºç®å½å±æ¬¡ç»æ åçéå¯æ å¿) kubectl æ§å¶ Kubernetes éç¾¤ç®¡çå¨ 