package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"

	"github.com/gin-gonic/gin"
	"github.com/juju/errors"
	"github.com/loopfz/gadgeto/tonic"
	"github.com/loopfz/gadgeto/tonic/utils/jujerr"
	"github.com/loopfz/gadgeto/zesty"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"github.com/wI2L/fizz"
	"github.com/wI2L/fizz/openapi"

	"github.com/cneill/utask"
	"github.com/cneill/utask/api/handler"
	"github.com/cneill/utask/db"
	"github.com/cneill/utask/models/resolution"
	"github.com/cneill/utask/models/task"
	"github.com/cneill/utask/pkg/auth"
)

type PluginRoute struct {
	Secured     bool
	Maintenance bool
	Path        string
	Method      string
	Infos       []fizz.OperationOption
	Handlers    []gin.HandlerFunc
}

type PluginRouterGroup struct {
	Path        string
	Name        string
	Description string
	Routes      []PluginRoute
}

// Server wraps the http handler that exposes a REST API to control
// the task orchestration engine
type Server struct {
	httpHandler            *fizz.Fizz
	authMiddleware         func(*gin.Context)
	dashboardPathPrefix    string
	dashboardAPIPathPrefix string
	dashboardSentryDSN     string
	maxBodyBytes           int64
	customMiddlewares      []gin.HandlerFunc
	pluginRoutes           []PluginRouterGroup
}

// NewServer returns a new Server
func NewServer() *Server {
	return &Server{
		authMiddleware: func(c *gin.Context) { c.Next() }, // default no-op middleware
	}
}

// WithAuth configures the Server's auth middleware
// it receives an authProvider function capable of extracting a caller's identity from an *http.Request
// the authProvider function also has discretion to deny authorization for a request by returning an error
func (s *Server) WithAuth(authProvider func(*http.Request) (string, error)) {
	if authProvider != nil {
		s.authMiddleware = authMiddleware(authProvider)
	}
}

// WithAuthGroup configures the Server's auth group middleware
// it receives an groupAuthProvider function capable of extracting the caller's groups and identity from an *http.Request
// the groupAuthProvider function also has discretion to deny authorization for a request by returning an error
func (s *Server) WithGroupAuth(groupAuthProvider func(*http.Request) (string, []string, error)) {
	if groupAuthProvider != nil {
		s.authMiddleware = groupAuthMiddleware(groupAuthProvider)
	}
}

// WithCustomMiddlewares sets an array of customized gin middlewares.
// It helps for init plugins to include these customized middlewares in the api server
func (s *Server) WithCustomMiddlewares(customMiddlewares ...gin.HandlerFunc) {
	s.customMiddlewares = append(s.customMiddlewares, customMiddlewares...)
}

// SetDashboardPathPrefix configures the custom path prefix for dashboard static files hosting.
// It doesn't change the path used by utask API to serve the files, it's only used inside UI files
// in order that dashboard can be aware of a ProxyPass configuration.
func (s *Server) SetDashboardPathPrefix(dashboardPathPrefix string) {
	s.dashboardPathPrefix = dashboardPathPrefix
}

// SetDashboardAPIPathPrefix configures a custom path prefix that UI should use when calling utask API.
// Required when utask API is exposed behind a ProxyPass and UI need to know the absolute URI to call.
func (s *Server) SetDashboardAPIPathPrefix(dashboardAPIPathPrefix string) {
	s.dashboardAPIPathPrefix = dashboardAPIPathPrefix
}

// SetDashboardSentryDSN configures a Sentry DSN URI to send UI exceptions and failures to.
func (s *Server) SetDashboardSentryDSN(dashboardSentryDSN string) {
	s.dashboardSentryDSN = dashboardSentryDSN
}

// SetMaxBodyBytes
func (s *Server) SetMaxBodyBytes(max int64) {
	s.maxBodyBytes = max
}

// ListenAndServe launches an http server and stays blocked until
// the server is shut down by a system signal
func (s *Server) ListenAndServe() error {
	ctx, cancel := context.WithCancel(context.Background())

	s.build(ctx)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	srv := &http.Server{Addr: fmt.Sprintf(":%d", utask.FPort), Handler: s.httpHandler}

	go func() {
		<-stop
		logrus.Info("Shutting down...")
		cancel()

		if err := srv.Shutdown(context.Background()); err != nil {
			logrus.Fatal(err)
		}
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Handler returns the underlying http.Handler of a Server
func (s *Server) Handler(ctx context.Context) http.Handler {
	s.build(ctx)
	return s.httpHandler
}

// RegisterPluginRoutes allows plugins to register custom routes
func (s *Server) RegisterPluginRoutes(group PluginRouterGroup) error {
	for _, route := range group.Routes {
		if len(route.Handlers) == 0 {
			return errors.NewNotImplemented(nil, "found route without handler")
		}
	}
	s.pluginRoutes = append(s.pluginRoutes, group)
	return nil
}

func generateBaseHref(pathPrefix, uri string) string {
	// UI requires to have a trailing slash at the end
	return path.Join(pathPrefix, uri) + "/"
}

func generatePathPrefixAPI(pathPrefix string) string {
	p := path.Join(pathPrefix, "/")
	if p == "." {
		p = "/"
	} else if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// build registers all routes and their corresponding handlers for the Server's API
func (s *Server) build(ctx context.Context) {
	if s.httpHandler == nil {
		ginEngine := gin.New()
		ginEngine.Use(gin.Recovery())

		ginEngine.
			Group("/",
				StaticFilePatternReplaceMiddleware(
					"___UTASK_DASHBOARD_BASEHREF___",
					generateBaseHref(s.dashboardPathPrefix, "/ui/dashboard"),
					"___UTASK_DASHBOARD_PREFIXAPIBASEURL___",
					generatePathPrefixAPI(s.dashboardAPIPathPrefix),
					"___UTASK_DASHBOARD_SENTRY_DSN___",
					s.dashboardSentryDSN)).
			StaticFS("/ui/dashboard", http.Dir("./static/dashboard"))

		ginEngine.
			Group("/",
				StaticFilePatternReplaceMiddleware(
					"___UTASK_DASHBOARD_PREFIXAPIBASEURL___",
					generatePathPrefixAPI(s.dashboardAPIPathPrefix),
					"___UTASK_DASHBOARD_SENTRY_DSN___",
					s.dashboardSentryDSN)).
			StaticFS("/ui/swagger", http.Dir("./static/swagger-ui"))

		collectMetrics(ctx)
		ginEngine.GET("/metrics", gin.WrapH(promhttp.Handler()))

		router := fizz.NewFromEngine(ginEngine)

		openapiPathPrefix := s.dashboardAPIPathPrefix
		if openapiPathPrefix == "" {
			openapiPathPrefix = "/"
		}
		router.Generator().SetServers([]*openapi.Server{
			{
				URL:         openapiPathPrefix,
				Description: utask.AppName(),
			},
		})

		router.Use(s.customMiddlewares...)
		router.Use(ajaxHeadersMiddleware, auditLogsMiddleware)

		tonic.SetErrorHook(jujerr.ErrHook)
		tonic.SetBindHook(defaultBindingHook(s.maxBodyBytes))
		tonic.SetRenderHook(yamljsonRenderHook, "application/json")

		authRoutes := router.Group("/", "x-misc", "Misc authenticated routes", s.authMiddleware)
		{
			templateRoutes := authRoutes.Group("/", "04 - template", "Manage uTask task templates")
			{
				// public template listing
				templateRoutes.GET("/template",
					[]fizz.OperationOption{
						fizz.ID("ListTemplates"),
						fizz.Summary("List task templates"),
					},
					tonic.Handler(handler.ListTemplates, 200))
				templateRoutes.GET("/template/:name",
					[]fizz.OperationOption{
						fizz.ID("GetTemplate"),
						fizz.Summary("Get task template details"),
					},
					tonic.Handler(handler.GetTemplate, 200))
			}

			functionRoutes := authRoutes.Group("/", "05 - function", "Manage uTask task functions")
			{
				// public function listing
				functionRoutes.GET("/function",
					[]fizz.OperationOption{
						fizz.ID("ListFunctions"),
						fizz.Summary("List task functions"),
					},
					tonic.Handler(handler.ListFunctions, 200))
				functionRoutes.GET("/function/:name",
					[]fizz.OperationOption{
						fizz.ID("GetFunction"),
						fizz.Summary("Get task function details"),
					},
					tonic.Handler(handler.GetFunction, 200))
			}

			// task
			taskRoutes := authRoutes.Group("/", "01 - task", "Manage uTask tasks")
			{
				// task creation in batches
				taskRoutes.POST("/batch",
					[]fizz.OperationOption{
						fizz.ID("BatchCreateTask"),
						fizz.Summary("Create a batch of tasks"),
					},
					maintenanceMode,
					tonic.Handler(handler.CreateBatch, 201))
				taskRoutes.POST("/task",
					[]fizz.OperationOption{
						fizz.ID("CreateTask"),
						fizz.Summary("Create new task"),
					},
					maintenanceMode,
					tonic.Handler(handler.CreateTask, 201))
				taskRoutes.GET("/task",
					[]fizz.OperationOption{
						fizz.ID("ListTasks"),
						fizz.Summary("List tasks"),
					},
					tonic.Handler(handler.ListTasks, 200))
				taskRoutes.GET("/task/:id",
					[]fizz.OperationOption{
						fizz.ID("GetTask"),
						fizz.Summary("Get task details"),
					},
					tonic.Handler(handler.GetTask, 200))
				taskRoutes.PUT("/task/:id",
					[]fizz.OperationOption{
						fizz.ID("EditTask"),
						fizz.Summary("Edit task"),
					},
					maintenanceMode,
					tonic.Handler(handler.UpdateTask, 200))
				taskRoutes.POST("/task/:id/wontfix",
					[]fizz.OperationOption{
						fizz.ID("CancelTask"),
						fizz.Summary("Cancel task"),
					},
					maintenanceMode,
					tonic.Handler(handler.WontfixTask, 204))
				taskRoutes.DELETE("/task/:id",
					[]fizz.OperationOption{
						fizz.ID("DeleteTask"),
						fizz.Summary("Delete task"),
						fizz.Description("Admin rights required"),
					},
					requireAdmin,
					maintenanceMode,
					tonic.Handler(handler.DeleteTask, 204))
			}

			// comments
			commentsRoutes := authRoutes.Group("/", "03 - comment", "Manage uTask task comments")
			{
				commentsRoutes.POST("/task/:id/comment",
					[]fizz.OperationOption{
						fizz.ID("AddTaskComment"),
						fizz.Summary("Post new comment on task"),
					},
					maintenanceMode,
					tonic.Handler(handler.CreateComment, 201))
				commentsRoutes.GET("/task/:id/comment",
					[]fizz.OperationOption{
						fizz.ID("ListTaskComments"),
						fizz.Summary("List task comments"),
					},
					tonic.Handler(handler.ListComments, 200))
				commentsRoutes.GET("/task/:id/comment/:commentid",
					[]fizz.OperationOption{
						fizz.ID("GetTaskComment"),
						fizz.Summary("Get single task comment"),
					},
					tonic.Handler(handler.GetComment, 200))
				commentsRoutes.PUT("/task/:id/comment/:commentid",
					[]fizz.OperationOption{
						fizz.ID("EditTaskComment"),
						fizz.Summary("Edit task comment"),
					},
					maintenanceMode,
					tonic.Handler(handler.UpdateComment, 200))
				commentsRoutes.DELETE("/task/:id/comment/:commentid",
					[]fizz.OperationOption{
						fizz.ID("DeleteTaskComment"),
						fizz.Summary("Delete task comment"),
					},
					maintenanceMode,
					tonic.Handler(handler.DeleteComment, 204))
			}

			// resolution
			resolutionRoutes := authRoutes.Group("/", "02 - resolution", "Manager uTask resolutions")
			{
				resolutionRoutes.POST("/resolution",
					[]fizz.OperationOption{
						fizz.ID("CreateTaskResolution"),
						fizz.Summary("Create task resolution"),
						fizz.Summary("This action instantiates a holder for the task's execution state. Only an approved resolver or admin user can perform this action."),
					},
					maintenanceMode,
					tonic.Handler(handler.CreateResolution, 201))
				resolutionRoutes.GET("/resolution/:id",
					[]fizz.OperationOption{
						fizz.ID("GetTaskResolution"),
						fizz.Summary("Get the details of a task resolution"),
						fizz.Description("Details include the intermediate results of every step. Admin users can view any resolution's details."),
					},
					tonic.Handler(handler.GetResolution, 200))
				resolutionRoutes.PUT("/resolution/:id",
					[]fizz.OperationOption{
						fizz.ID("EditTaskResolution"),
						fizz.Summary("Edit a task's resolution during execution."),
						fizz.Description("Action of last resort if a task needs fixing. Admin users only."),
					},
					requireAdmin,
					maintenanceMode,
					tonic.Handler(handler.UpdateResolution, 204))
				resolutionRoutes.POST("/resolution/:id/run",
					[]fizz.OperationOption{
						fizz.ID("ExecuteTask"),
						fizz.Summary("Execute a task"),
					},
					maintenanceMode,
					tonic.Handler(handler.RunResolution, 204))
				resolutionRoutes.POST("/resolution/:id/pause",
					[]fizz.OperationOption{
						fizz.ID("PauseTaskExecution"),
						fizz.Summary("Pause a task's execution"),
						fizz.Description("This action takes a task out of the execution pipeline, it will not be considered for automatic retry until it is re-run manually."),
					},
					maintenanceMode,
					tonic.Handler(handler.PauseResolution, 204))
				resolutionRoutes.POST("/resolution/:id/extend",
					[]fizz.OperationOption{
						fizz.ID("ExtendTaskResolution"),
						fizz.Summary("Extend max retry limit for a task's execution"),
					},
					maintenanceMode,
					tonic.Handler(handler.ExtendResolution, 204))
				resolutionRoutes.POST("/resolution/:id/cancel",
					[]fizz.OperationOption{
						fizz.ID("CancelTaskResolution"),
						fizz.Summary("Cancel a task's execution"),
					},
					maintenanceMode,
					tonic.Handler(handler.CancelResolution, 204))
				resolutionRoutes.GET("/resolution/:id/step/:stepName",
					[]fizz.OperationOption{
						fizz.ID("GetTaskResolutionStep"),
						fizz.Summary("Get the details of the step of a task resolution"),
						fizz.Description("Returns the current implementation of the step, including the output of the step."),
					},
					tonic.Handler(handler.GetResolutionStep, 200))
				resolutionRoutes.PUT("/resolution/:id/step/:stepName",
					[]fizz.OperationOption{
						fizz.ID("EditTaskResolutionStep"),
						fizz.Summary("Edit the details of the step of a task resolution"),
						fizz.Description("Allow the edition of a step, if a step needs fixing. Admin users only."),
					},
					requireAdmin,
					maintenanceMode,
					tonic.Handler(handler.UpdateResolutionStep, 204))
				resolutionRoutes.PUT("/resolution/:id/step/:stepName/state",
					[]fizz.OperationOption{
						fizz.ID("EditTaskResolutionStepState"),
						fizz.Summary("Edit the state of the step of a task resolution"),
						fizz.Description("Allow the edition of the step state, if a step needs to be re-run or skipped manually. Resolution managers only."),
					},
					maintenanceMode,
					tonic.Handler(handler.UpdateResolutionStepState, 204))

				//	resolutionRoutes.POST("/resolution/:id/rollback",
				//		[]fizz.OperationOption{
				// 			fizz.Summary(""),
				//		},
				//		tonic.Handler(handler.ResolutionRollback, 200))
			}

			authRoutes.GET("/",
				[]fizz.OperationOption{
					fizz.Summary("Redirect to /meta"),
				},
				func(c *gin.Context) {
					c.Redirect(http.StatusMovedPermanently, "/meta")
				})

			authRoutes.GET("/meta",
				[]fizz.OperationOption{
					fizz.ID("GetMetadata"),
					fizz.Summary("Display service name and user's status"),
				},
				tonic.Handler(rootHandler, 200))

			// admin
			authRoutes.POST("/key-rotate",
				[]fizz.OperationOption{
					fizz.ID("ReencryptData"),
					fizz.Summary("Re-encrypt all data with latest storage key"),
				},
				requireAdmin,
				tonic.Handler(keyRotate, 200))
		}

		router.GET("/unsecured/mon/ping",
			[]fizz.OperationOption{
				fizz.Summary("Assert that the service is running and can talk to it's data backend"),
			},
			pingHandler)
		router.GET("/unsecured/spec.json", nil, router.OpenAPI(&openapi.Info{
			Title:   utask.AppName(),
			Version: utask.Version,
		}, "json"))
		router.GET("/unsecured/stats",
			[]fizz.OperationOption{
				fizz.Summary("Fetch statistics about existing tasks"),
			},
			tonic.Handler(Stats, 200))

		// plugin routes
		for _, p := range s.pluginRoutes {
			group := router.Group(p.Path, p.Name, p.Description)

			for _, r := range p.Routes {
				routeHandlers := []gin.HandlerFunc{}

				if r.Maintenance {
					routeHandlers = append(routeHandlers, maintenanceMode)
				}
				if r.Secured {
					routeHandlers = append(routeHandlers, s.authMiddleware)
				}

				routeHandlers = append(routeHandlers, r.Handlers...)

				group.Handle(r.Path, r.Method, r.Infos, routeHandlers...)
			}
		}

		s.httpHandler = router
	}
}

func pingHandler(c *gin.Context) {
	dbp, err := zesty.NewDBProvider(utask.DBName)
	if err != nil {
		c.String(http.StatusInternalServerError, "")
		c.Error(err)
		return
	}
	i, err := dbp.DB().SelectInt(`SELECT 1`)
	if err != nil {
		c.String(http.StatusInternalServerError, "")
		c.Error(err)
		return
	}
	if i != 1 {
		c.String(http.StatusInternalServerError, "")
		c.Error(fmt.Errorf("Unexpected value %d", i))
		return
	}
	c.String(http.StatusOK, "pong")
}

type rootOut struct {
	ApplicationName string   `json:"application_name"`
	UserIsAdmin     bool     `json:"user_is_admin"`
	Username        string   `json:"username"`
	UserGroups      []string `json:"user_groups"`
	Version         string   `json:"version"`
	Commit          string   `json:"commit"`
}

func rootHandler(c *gin.Context) (*rootOut, error) {
	groups := auth.GetGroups(c)
	if groups == nil {
		groups = []string{}
	}

	return &rootOut{
		ApplicationName: utask.AppName(),
		UserIsAdmin:     auth.IsAdmin(c) == nil,
		Username:        auth.GetIdentity(c),
		UserGroups:      groups,
		Version:         utask.Version,
		Commit:          utask.Commit,
	}, nil
}

func requireAdmin(c *gin.Context) {
	if err := auth.IsAdmin(c); err != nil {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	c.Next()
}

func maintenanceMode(c *gin.Context) {
	if utask.FMaintenanceMode {
		c.JSON(http.StatusMethodNotAllowed, map[string]string{
			"error": "Maintenance mode activated",
		})
		return
	}
	c.Next()
}

func keyRotate(c *gin.Context) error {
	dbp, err := zesty.NewDBProvider(utask.DBName)
	if err != nil {
		return err
	}
	if err := db.CallKeyRotations(dbp); err != nil {
		return err
	}
	if err := task.RotateTasks(dbp); err != nil {
		return err
	}
	return resolution.RotateResolutions(dbp)
}
