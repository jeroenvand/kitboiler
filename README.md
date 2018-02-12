# KitBoiler

KitBoiler generates endpoints, request/response types, request decoders and http handlers 
for use with Go kit (https://gokit.io), based on an interface that defines a service.

*NOTE: KitBoiler is still work in progress and may or may not work as advertised. It probably won't eat your cat although you can't be sure until it does.*  

## Usage

Given a service definition/interface in github.com/me/mypkg/api/somefile.go:

    type MyService interface {
        MyFirstFunction(name string) (err error)
        MyFirstQuery() (results []*model.QueryResult, err error)
        MySecondQuery() (result *somepkg.FooBar, err error)
    }

NOTE: you HAVE to provide names for both the parameters and the return vars in your interface definition as
those are used by kitboiler. Choose the names wisely as they will become part of your public interface.

You should call KitBoiler like:

    kitboiler github.com/me/mypkg/api.MyService 

This generates a package containing endpoint functions, request/response types and
http handler functions for all functions defined in the interface specification.

Implementation is based on the impl package by Josh Snyder (https://github.com/josharian/impl) and inspiration was generously provided 
by SQLBoiler (https://github.com/volatiletech/sqlboiler)